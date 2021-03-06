package log

import (
	"encoding/base64"
	"fmt"
	"time"
)

// Valuer is an interface to deal with typing problems of just having an interface{} as the acceptable parameters
// Go-kit logging has a failure case if you attempt to throw any values into it.
// This is a way to guard our developers from having to worry about error cases of the lower logging framework.
type Valuer interface {
	getValue() interface{}
}

type any struct {
	value interface{}
}

func (a *any) getValue() interface{} {
	return a.value
}

func String(s string) Valuer {
	return &any{s}
}

func Int(i int) Valuer {
	return &any{i}
}

func Float64(f float64) Valuer {
	return &any{f}
}

func Bool(b bool) Valuer {
	return &any{b}
}

func TimeDuration(d time.Duration) Valuer {
	return &any{d.String()}
}

func Time(t time.Time) Valuer {
	return TimeFormatted(t, time.RFC3339Nano)
}

func TimeFormatted(t time.Time, format string) Valuer {
	return String(t.Format(format))
}

func ByteString(b []byte) Valuer {
	return String(string(b))
}

func ByteBase64(b []byte) Valuer {
	return String(base64.RawURLEncoding.EncodeToString(b))
}

func Stringer(s fmt.Stringer) Valuer {
	return &any{s.String()}
}
