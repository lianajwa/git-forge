// JSON validity helper (standard library; only used on the malformed-body
// path, never on the hot workloads).
package main

import "encoding/json"

func jsonValid(b []byte) bool { return json.Valid(b) }
