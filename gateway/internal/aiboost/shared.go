package aiboost

import (
 "context"
 "encoding/json"
 "errors"
 "net/url"
 "time"
)

// Sharing transports only sealed connection records and coordinates refresh.
// Implementations must fence Begin/Complete and durably record pending refresh
// before Begin returns; a pending operation is never retried by a successor.
type Sharing interface {
 Load() ([]byte, int, error)
 Save([]byte, int, uint64, bool) (int, error)
 Claim(int) (uint64, error)
 Begin(int, uint64) error
}

var ErrRefreshWaiting = errors.New("AI Boost shared refresh unavailable; retry later")

func (c *Client) SetSharing(s Sharing) {
 c.mu.Lock()
 defer c.mu.Unlock()
 c.sharing = s
}

func (c *Client) syncShared() error {
 if c.sharing == nil { return nil }
 raw, version, err := c.sharing.Load()
 if err != nil { return err }
 if version == 0 {
  if c.rec.Status == "ok" && c.rec.RefreshToken != "" { return c.publishShared(false, 0) }
  return nil
 }
 if version < c.sharedVersion { return ErrRefreshWaiting }
 var r record
 if json.Unmarshal(raw, &r) != nil || validateConfig(r.BaseURL, r.RedirectURI) != nil || r.ClientID == "" {
  return errors.New("invalid shared AI Boost connection")
 }
 if err := c.commit(r); err != nil { return err }
 c.sharedVersion = version
 return nil
}

func (c *Client) publishShared(complete bool, epoch uint64) error {
 if c.sharing == nil { return nil }
 raw, err := json.Marshal(c.rec)
 if err != nil { return err }
 version, err := c.sharing.Save(raw, c.sharedVersion, epoch, complete)
 if err == nil { c.sharedVersion = version }
 return err
}

// Maintain proactively rotates before access expiry. Every enrolled gateway is
// eligible: no worker or hull requirement is involved.
func (c *Client) Maintain(ctx context.Context) error {
 c.mu.Lock()
 defer c.mu.Unlock()
 if err := c.syncShared(); err != nil { return err }
 if c.rec.Status != "ok" { return nil }
 if time.Now().Add(120*time.Second).Before(c.rec.AccessExpires) { return nil }
 return c.refresh(ctx)
}

func (c *Client) sharedRefresh(ctx context.Context) error {
 if err := c.syncShared(); err != nil { return err }
 if c.rec.Status != "ok" || c.rec.RefreshPending || !time.Now().Before(c.rec.RefreshExpires) { return ErrReconnect }
 epoch, err := c.sharing.Claim(c.sharedVersion)
 if err != nil { return err }
 if err = c.sharing.Begin(c.sharedVersion, epoch); err != nil { return err }
 // Begin is durably marked remotely before the single-use token is sent.
 r := c.rec
 r.RefreshPending = true
 if err = c.commit(r); err != nil { return err }
 tok, err := c.token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {r.RefreshToken}})
 if err != nil {
  _ = c.disconnect("needs-auth")
  _ = c.publishShared(true, epoch)
  return ErrReconnect
 }
 if err = c.apply(tok); err != nil { return err }
 // Completion is version- and epoch-fenced. A lost response is recovered by
 // loading the authoritative envelope, never by replaying token exchange.
 return c.publishShared(true, epoch)
}
