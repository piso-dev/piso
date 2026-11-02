package technocore

import (
 "encoding/base64"
 "encoding/json"
 "fmt"
 "net/http"
 "strings"

 "piso/gateway/internal/aiboost"
 tc "piso/internal/technocore"
)

// BoostSharing keeps OAuth plaintext out of state.json and workers. Only the
// AI Boost client's encrypted local vault materializes the sealed bundle.
type BoostSharing struct { DataDir string }

func (s BoostSharing) identity() (tc.Identity, error) { return tc.Load(tc.Path(s.DataDir)) }

func (s BoostSharing) Load() ([]byte, int, error) {
 id, err := s.identity(); if err != nil { return nil, 0, err }
 code, raw, err := Call(id, http.MethodGet, "/api/v1/sync/secrets", nil, nil)
 if err != nil || code != 200 { return nil, 0, fmt.Errorf("AI Boost shared connection unavailable") }
 var out struct { Objects []envelope `json:"objects"` }
 if json.Unmarshal(raw, &out) != nil { return nil, 0, fmt.Errorf("invalid shared connection response") }
 for _, obj := range out.Objects {
  if obj.Metadata["kind"] != "ai-boost" { continue }
  if obj.Status != "active" { return nil, 0, aiboost.ErrReconnect }
  wrap, e1 := base64.StdEncoding.DecodeString(obj.WrappedDataKey)
  nonce, e2 := base64.StdEncoding.DecodeString(obj.Nonce)
  cipher, e3 := base64.StdEncoding.DecodeString(obj.Ciphertext)
  if e1 != nil || e2 != nil || e3 != nil { return nil, 0, fmt.Errorf("invalid shared connection envelope") }
  plain, err := tc.OpenEnvelope(id, wrap, nonce, cipher)
  if err != nil || plaintextHash(string(plain)) != obj.ContentHash { return nil, 0, fmt.Errorf("invalid shared connection ciphertext") }
  return plain, obj.ContentVersion, nil
 }
 return nil, 0, nil
}

func (s BoostSharing) Save(raw []byte, version int, epoch uint64, complete bool) (int, error) {
 id, err := s.identity(); if err != nil { return 0, err }
 gateways, err := fetchGateways(id); if err != nil { return 0, err }
 operation := "save"; if complete { operation = "complete" }
 in := ShareInput{Name:"AI Boost", Kind:"ai-boost", GatewayOnly:true, Plaintext:string(raw), BaseVersion:version,
  Operation:operation, LeaseEpoch:epoch}
 out, _, err := shareWith(id, gateways, in, http.MethodPost, strings.TrimRight(id.ServerURL,"/")+"/api/v1/ai-boost/connection")
 if err != nil { return 0, fmt.Errorf("AI Boost shared publication failed") }
 v, ok := out["content_version"].(float64); if !ok { return 0, fmt.Errorf("invalid shared connection version") }
 return int(v), nil
}

func (s BoostSharing) lease(version int, epoch uint64, operation string) (uint64, error) {
 id, err := s.identity(); if err != nil { return 0, err }
 body, _ := json.Marshal(map[string]any{"base_version":version,"lease_epoch":epoch,"operation":operation})
 code, raw, err := Call(id,http.MethodPost,"/api/v1/ai-boost/refresh-lease",nil,body)
 if err != nil || code != 200 { return 0, aiboost.ErrRefreshWaiting }
 var out struct { Granted bool `json:"granted"`; Epoch uint64 `json:"lease_epoch"`; Reason string `json:"reason"` }
 if json.Unmarshal(raw,&out) != nil { return 0, aiboost.ErrRefreshWaiting }
 if !out.Granted { if out.Reason == "needs-auth" { return 0,aiboost.ErrReconnect }; return 0,aiboost.ErrRefreshWaiting }
 return out.Epoch,nil
}
func (s BoostSharing) Claim(v int) (uint64,error) { return s.lease(v,0,"claim") }
func (s BoostSharing) Begin(v int,e uint64) error { _,err:=s.lease(v,e,"begin"); return err }
