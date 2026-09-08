package doltserver

import "fmt"

type ListenerTimeouts struct {
	ReadMs  int
	WriteMs int
}

func ResolveListenerTimeouts(townRoot string, defaultReadMs, defaultWriteMs int) ListenerTimeouts {
	return ListenerTimeouts{
		ReadMs:  resolveListenerTimeoutMs(townRoot, "GT_DOLT_READ_TIMEOUT_MS", defaultReadMs),
		WriteMs: resolveListenerTimeoutMs(townRoot, "GT_DOLT_WRITE_TIMEOUT_MS", defaultWriteMs),
	}
}

func (t ListenerTimeouts) YAML() string {
	lines := ""
	if t.ReadMs > 0 {
		lines += fmt.Sprintf("\n  read_timeout_millis: %d", t.ReadMs)
	}
	if t.WriteMs > 0 {
		lines += fmt.Sprintf("\n  write_timeout_millis: %d", t.WriteMs)
	}
	return lines
}
