package bps

import (
	"bytes"
	"io"
)

// Only credentials supplied by this bridge are removed, not arbitrary upstream details.
func redactCurrentCredentials(data []byte, secrets []string) []byte {
	for _, secret := range secrets {
		if secret != "" {
			data = bytes.ReplaceAll(data, []byte(secret), []byte("[REDACTED]"))
		}
	}
	return data
}
func copyErrorBody(dst io.Writer, src io.Reader, secrets []string) error {
	keep := 0
	for _, s := range secrets {
		if len(s) > keep {
			keep = len(s)
		}
	}
	buf := make([]byte, 32768)
	pending := []byte{}
	for {
		n, err := src.Read(buf)
		pending = append(pending, buf[:n]...)
		cut := len(pending) - keep
		if err != nil {
			cut = len(pending)
		}
		if cut > 0 {
			if err == nil {
				for _, s := range secrets {
					if s != "" {
						start := bytes.Index(pending, []byte(s))
						if start >= 0 && start < cut && start+len(s) > cut {
							cut = start
						}
					}
				}
			}
			if _, e := dst.Write(redactCurrentCredentials(pending[:cut], secrets)); e != nil {
				return e
			}
			pending = append(pending[:0], pending[cut:]...)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
