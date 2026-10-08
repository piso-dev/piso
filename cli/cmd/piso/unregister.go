package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func unregisterWorkerWithGateway(gateway, name string) error {
	req, err := http.NewRequest(http.MethodDelete, gateway+"/api/v1/workers/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("gateway %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
