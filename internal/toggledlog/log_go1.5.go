// +build go1.5
// = go 1.5 or higher

package toggledlog

import (
	"log/syslog"
)

func (l *toggledLogger) SwitchToSyslog(p syslog.Priority) {
	w, err := syslog.New(p, ProgramName)
	if err != nil {
		Warn.Printf("Cannot switch 0x%02x to syslog: %v", p, err)
	} else {
		l.SetOutput(w)
	}
}
