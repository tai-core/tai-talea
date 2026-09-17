package obs

import (
	"bytes"
	"io"
)

func jsonReader(payload []byte) io.Reader { return bytes.NewReader(payload) }
