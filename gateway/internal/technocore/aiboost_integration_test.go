package technocore

import (
 "context"
 "encoding/base64"
 "encoding/json"
 "net/http"
 "net/http/httptest"
 "net/url"
 "path/filepath"
 "sync"
 "sync/atomic"
 "testing"
 "time"

 "piso/gateway/internal/aiboost"
 tc "piso/internal/technocore"
)

// Exercise the production envelope adapter and OAuth client, rather than a
// fake Sharing implementation. The coordinator fixture stores ciphertext only.
func TestBoostSharingEndToEnd(t *testing.T) {
 var refreshes, calls atomic.Int32
 const redirect = "https://piso.example/oauth/ai-boost/callback"
 provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
  switch r.URL.Path {
  case "/oauth/register/":
   w.WriteHeader(201)
   json.NewEncoder(w).Encode(map[string]any{"client_id":"client","client_secret":"secret","redirect_uris":[]string{redirect}})
  case "/oauth/token/":
   r.ParseForm()
   access,refresh := "first-access","first-refresh"
   if r.Form.Get("grant_type") == "refresh_token" {
    if r.Form.Get("refresh_token") != "first-refresh" { t.Error("reused or wrong refresh token") }
    refreshes.Add(1); access,refresh = "second-access","second-refresh"
   }
   json.NewEncoder(w).Encode(map[string]any{"access_token":access,"refresh_token":refresh,"expires_in":900,
    "refresh_token_expires_in":86400,"scope":aiboost.Scope,"token_type":"Bearer"})
  case "/api/library/boosters/":
   want := "Bearer first-access"; if refreshes.Load() > 0 { want = "Bearer second-access" }
   if r.Header.Get("Authorization") != want { t.Errorf("bearer = %q, want %q",r.Header.Get("Authorization"),want) }
   calls.Add(1); json.NewEncoder(w).Encode(map[string]any{"items":[]any{}})
  default: http.NotFound(w,r)
  }
 }))
 defer provider.Close()
 var mu sync.Mutex
 var version int
 var ciphertext,nonce,digest string
 wraps := map[string]string{}
 var holder string
 var pending bool
 var epoch uint64
 var ids []tc.Identity
 coordinator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
  mu.Lock(); defer mu.Unlock()
  pub := r.Header.Get("X-Piso-Pubkey")
  var caller tc.Identity
  for _,id := range ids { if id.SigningPublicKey == pub { caller = id } }
  if caller.PrincipalID == "" { w.WriteHeader(401); return }
  var body map[string]any
  if r.Method == http.MethodPost { json.NewDecoder(r.Body).Decode(&body) }
  switch r.URL.Path {
  case "/api/v1/gateways":
   var gateways []remoteGateway
   for _,id := range ids { gateways = append(gateways, remoteGateway{PrincipalID:id.PrincipalID, EncryptionPubKey:id.EncryptionPublicKey,Status:"active"}) }
   json.NewEncoder(w).Encode(map[string]any{"gateways":gateways})
  case "/api/v1/sync/secrets":
   var objects []envelope
   if wrap := wraps[caller.PrincipalID]; wrap != "" {
    objects = append(objects,envelope{ResourceID:"connection",Status:"active",Metadata:map[string]any{"kind":"ai-boost"},
     Algorithm:tc.EnvelopeAlg,ContentVersion:version,ContentHash:digest,Ciphertext:ciphertext,Nonce:nonce,WrappedDataKey:wrap})
   }
   json.NewEncoder(w).Encode(map[string]any{"objects":objects})
  case "/api/v1/ai-boost/connection":
   base,_ := body["base_version"].(float64)
   if int(base) != version { w.WriteHeader(409); return }
   if body["operation"] == "complete" {
    e,_:=body["lease_epoch"].(float64)
    if !pending || holder != caller.PrincipalID || uint64(e)!=epoch { w.WriteHeader(409); return }
   }
   ciphertext,_=body["ciphertext"].(string); nonce,_=body["nonce"].(string);digest,_=body["content_hash"].(string)
   wraps=map[string]string{}
   for _,v:=range body["wraps"].([]any) { item:=v.(map[string]any); wraps[item["principal_id"].(string)]=item["wrapped_data_key"].(string) }
   version++;pending=false;epoch++
   json.NewEncoder(w).Encode(map[string]any{"resource_id":"connection","content_version":version})
  case "/api/v1/ai-boost/refresh-lease":
   if pending { json.NewEncoder(w).Encode(map[string]any{"granted":false,"reason":"refresh_pending"});return }
   if body["operation"]=="begin" { pending=true } else { holder=caller.PrincipalID;epoch++ }
   json.NewEncoder(w).Encode(map[string]any{"granted":true,"lease_epoch":epoch})
  default: http.NotFound(w,r)
  }
 }))
 defer coordinator.Close()
 var clients []*aiboost.Client
 for i:=0;i<2;i++ {
  dir:=t.TempDir()
  id,err:=tc.Generate(coordinator.URL,"test");if err!=nil {t.Fatal(err)}
  id.Status=tc.StatusActive;id.PrincipalID=[]string{"gateway-a","gateway-b"}[i]
  if err=tc.Save(tc.Path(dir),id);err!=nil {t.Fatal(err)}
  ids=append(ids,id)
  c,err:=aiboost.Open(filepath.Join(dir,"aiboost"),provider.Client());if err!=nil {t.Fatal(err)}
  c.SetSharing(BoostSharing{DataDir:dir});clients=append(clients,c)
 }
 a,b:=clients[0],clients[1]
 if err:=a.Configure(context.Background(),provider.URL,redirect);err!=nil {t.Fatal(err)}
 start,err:=a.Start("browser");if err!=nil {t.Fatal(err)}
 u,_:=url.Parse(start)
 if err=a.Callback(context.Background(),u.Query().Get("state"),"browser","code","");err!=nil {t.Fatal(err)}
 for _,c:=range clients { if _,err=c.List(context.Background(),"","");err!=nil {t.Fatal(err)} }
 if version!=1 || calls.Load()!=2 || b.Summary().Status!="ok" {t.Fatal("connection did not propagate")}
 // Age the authoritative encrypted record without touching either local vault.
 mu.Lock()
 wrap,_:=base64.StdEncoding.DecodeString(wraps[ids[0].PrincipalID]);n,_:=base64.StdEncoding.DecodeString(nonce);ct,_:=base64.StdEncoding.DecodeString(ciphertext)
 plain,err:=tc.OpenEnvelope(ids[0],wrap,n,ct);if err!=nil {t.Fatal(err)}
 var record map[string]any;json.Unmarshal(plain,&record)
 record["accessExpires"]=time.Now().Add(90*time.Second).UTC().Format(time.RFC3339Nano)
 plain,_=json.Marshal(record)
 dek,n,ct,err:=tc.SealPayload(plain);if err!=nil {t.Fatal(err)}
 for _,id:=range ids {w,err:=tc.WrapDEK(id.EncryptionPublicKey,dek);if err!=nil {t.Fatal(err)};wraps[id.PrincipalID]=base64.StdEncoding.EncodeToString(w)}
 nonce=base64.StdEncoding.EncodeToString(n);ciphertext=base64.StdEncoding.EncodeToString(ct);digest=plaintextHash(string(plain));version++
 mu.Unlock()
 if err=a.Maintain(context.Background());err!=nil {t.Fatal(err)}
 if err=b.Maintain(context.Background());err!=nil {t.Fatal(err)}
 if _,err=b.List(context.Background(),"","");err!=nil {t.Fatal(err)}
 if refreshes.Load()!=1 || version!=3 {t.Fatalf("refreshes=%d version=%d",refreshes.Load(),version)}
 if err=a.Disconnect();err!=nil {t.Fatal(err)}
 if b.Summary().Status!="none" {t.Fatal("disconnect did not propagate")}
}
