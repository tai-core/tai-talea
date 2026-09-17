package partner

import "encoding/json"

// Thin indirections keep the partner package free of struct tags leaking into
// helper call sites and make the JSON dependency explicit.
func jsonMarshal(value any) ([]byte, error)       { return json.Marshal(value) }
func jsonUnmarshal(data string, target any) error { return json.Unmarshal([]byte(data), target) }
