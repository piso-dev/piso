package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"piso/cli/internal/pisoconfig"
)

func cmdSecretsShare(args []string) error {
	fs := flag.NewFlagSet("secrets share", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "secret name")
	placeholder := fs.String("placeholder", "", "worker placeholder")
	envKey := fs.String("env", "", "env key exported to workers")
	hosts := fs.String("hosts", "", "comma-separated allowed hosts")
	if err := fs.Parse(args); err != nil {
		return errors.New("usage: piso secrets share --name n --placeholder piso_… --env KEY --hosts host [,host]")
	}
	if *name == "" || *placeholder == "" {
		return errors.New("usage: piso secrets share --name n --placeholder piso_… [--env KEY] [--hosts host]")
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	value := strings.TrimRight(string(raw), "\n")
	if value == "" {
		return errors.New("read secret value from stdin")
	}
	body := map[string]any{
		"name":         *name,
		"placeholder":  *placeholder,
		"envKey":       *envKey,
		"allowedHosts": splitCSV([]string{*hosts}),
		"value":        value,
	}
	return postTechnocore("/api/v1/technocore/share", body)
}

func cmdSecretsPublish(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: piso secrets publish <placeholder>")
	}
	return postTechnocore("/api/v1/technocore/publish", map[string]any{
		"placeholder": args[0],
	})
}

func postTechnocore(path string, body any) error {
	gw := pisoconfig.ControlAPIURL()
	if !healthy(gw) {
		return errors.New("local gateway is not up (run `piso up`)")
	}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(gw+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 201 && resp.StatusCode != 200 {
		return fmt.Errorf("gateway: %s", strings.TrimSpace(string(out)))
	}
	fmt.Println(strings.TrimSpace(string(out)))
	return nil
}
