package herdr

// types_gen.go holds the herdr API types, generated from the checked-in
// `herdr api schema --json` output. The selection lives in
// schemagen/herdrcron.go; the Go mapping rules are in package schemagen.
//go:generate go run ./cmd/herdr-schemagen -schema schema/herdr-0.9.1.json -out types_gen.go -pkg herdr
