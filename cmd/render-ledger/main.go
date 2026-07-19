package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/hexagon-codes/hexclaw/release/renderledger"
)

func main() {
	rows, err := renderledger.Build()
	if err == nil {
		err = renderledger.ValidateCurrent(rows)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	payload := struct {
		SchemaVersion string             `json:"schema_version"`
		Rows          []renderledger.Row `json:"rows"`
	}{SchemaVersion: "1.0", Rows: rows}
	if err := json.NewEncoder(os.Stdout).Encode(payload); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
