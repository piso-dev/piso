package mcpchannel

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudflare/circl/hpke"
)

func testKeys(t *testing.T) (gwPub, gwPriv []byte, wPub ed25519.PublicKey, wPriv ed25519.PrivateKey) {
	t.Helper()
	_, sk, err := hpke.KEM_X25519_HKDF_SHA256.Scheme().GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	gwPriv, err = sk.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	gwPub, err = sk.Public().MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	wPub, wPriv, err = ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return gwPub, gwPriv, wPub, wPriv
}

func TestSealOpenRoundTrip(t *testing.T) {
	gwPub, gwPriv, wPub, wPriv := testKeys(t)
	content := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	body, ctx, err := SealRequest(gwPub, wPub, wPriv, content, time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	req, err := OpenRequest(gwPriv, body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(req.Content, content) {
		t.Fatalf("content %q", req.Content)
	}
	if !req.Verify(gwPub) {
		t.Fatal("signature should verify")
	}
	if hex.EncodeToString(req.KeyID) != KeyIDHex(gwPub) {
		t.Fatal("key id mismatch")
	}
	resp, err := req.SealResponse([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := ctx.OpenResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if string(pt) != `{"jsonrpc":"2.0","id":1,"result":{}}` {
		t.Fatalf("response %s", pt)
	}
}

func TestTamperRejected(t *testing.T) {
	gwPub, gwPriv, wPub, wPriv := testKeys(t)
	content := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	body, _, err := SealRequest(gwPub, wPub, wPriv, content, time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(body); i++ {
		bad := append([]byte(nil), body...)
		bad[i] ^= 0x01
		if _, err := OpenRequest(gwPriv, bad); err == nil {
			// Flipping a byte inside key_id still opens (AAD binds it, so
			// open must fail). Every outer byte is covered by version, AAD,
			// enc, or ciphertext.
			t.Fatalf("tamper at %d accepted", i)
		}
	}
	req, err := OpenRequest(gwPriv, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Content = append([]byte(nil), req.Content...)
	req.Content[0] ^= 0x01
	if req.Verify(gwPub) {
		t.Fatal("content tamper should fail verify")
	}
	req.Content[0] ^= 0x01
	req.Sig[0] ^= 0x01
	if req.Verify(gwPub) {
		t.Fatal("sig tamper should fail verify")
	}
	req.Sig[0] ^= 0x01
	req.Enc[0] ^= 0x01
	if req.Verify(gwPub) {
		t.Fatal("enc tamper should fail verify")
	}
}

func TestWrongGatewayKey(t *testing.T) {
	gwPub, _, wPub, wPriv := testKeys(t)
	_, otherPriv, _, _ := testKeys(t)
	body, _, err := SealRequest(gwPub, wPub, wPriv, []byte(`{}`), time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRequest(otherPriv, body); err == nil {
		t.Fatal("wrong gateway key should fail")
	}
}

func TestSignedForOtherGatewayRejected(t *testing.T) {
	gwA, _, wPub, wPriv := testKeys(t)
	gwB, gwBPriv, _, _ := testKeys(t)
	body, _, err := SealRequest(gwA, wPub, wPriv, []byte(`{"m":1}`), time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	// Gateway B cannot open a request sealed to A. Even if the ciphertext
	// were transplanted, Verify binds the gateway public key.
	if _, err := OpenRequest(gwBPriv, body); err == nil {
		t.Fatal("gateway B opened a request sealed to A")
	}
	// Reconstruct the signature check: a request opened by A does not verify
	// against B's public key.
	_, gwAPriv, _, _ := mustReuse(t, gwA)
	// gwA private is unknown here; seal a fresh request to B and check the
	// signature does not verify under A's public key.
	bodyB, _, err := SealRequest(gwB, wPub, wPriv, []byte(`{"m":1}`), time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	req, err := OpenRequest(gwBPriv, bodyB)
	if err != nil {
		t.Fatal(err)
	}
	if req.Verify(gwA) {
		t.Fatal("request signed for B verified under A")
	}
	if !req.Verify(gwB) {
		t.Fatal("request should verify under B")
	}
	_ = gwAPriv
}

func mustReuse(t *testing.T, pub []byte) ([]byte, []byte, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	return pub, nil, nil, nil
}

func TestResponseBoundToRequestContext(t *testing.T) {
	gwPub, gwPriv, wPub, wPriv := testKeys(t)
	body1, ctx1, err := SealRequest(gwPub, wPub, wPriv, []byte(`{"id":1}`), time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	_, ctx2, err := SealRequest(gwPub, wPub, wPriv, []byte(`{"id":2}`), time.Unix(1_700_000_001, 0))
	if err != nil {
		t.Fatal(err)
	}
	req1, err := OpenRequest(gwPriv, body1)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := req1.SealResponse([]byte(`{"id":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ctx2.OpenResponse(resp); err == nil {
		t.Fatal("other request context opened the response")
	}
	if _, err := ctx1.OpenResponse(resp); err != nil {
		t.Fatal(err)
	}
}

func TestGoldenVector(t *testing.T) {
	path := filepath.Join("testdata", "golden.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		GatewayPub  string `json:"gatewayPub"`
		GatewayPriv string `json:"gatewayPriv"`
		WorkerPub   string `json:"workerPub"`
		WorkerPriv  string `json:"workerPriv"`
		Content     string `json:"content"`
		TS          int64  `json:"ts"`
		Nonce       string `json:"nonce"`
		Enc         string `json:"enc"`
		Request     string `json:"request"`
		Response    string `json:"response"`
		ResponsePT  string `json:"responsePlaintext"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	gwPub, _ := hex.DecodeString(v.GatewayPub)
	gwPriv, _ := hex.DecodeString(v.GatewayPriv)
	body, _ := hex.DecodeString(v.Request)
	req, err := OpenRequest(gwPriv, body)
	if err != nil {
		t.Fatal(err)
	}
	if string(req.Content) != v.Content {
		t.Fatalf("content %q", req.Content)
	}
	if !req.Verify(gwPub) {
		t.Fatal("golden signature")
	}
	if hex.EncodeToString(req.Enc) != v.Enc {
		t.Fatalf("enc %s", hex.EncodeToString(req.Enc))
	}
	if hex.EncodeToString(req.Nonce) != v.Nonce {
		t.Fatalf("nonce %s", hex.EncodeToString(req.Nonce))
	}
	respBody, _ := hex.DecodeString(v.Response)
	opened, err := req.OpenSealedResponse(respBody)
	if err != nil {
		t.Fatal(err)
	}
	if string(opened) != v.ResponsePT {
		t.Fatalf("opened %s", opened)
	}
}
