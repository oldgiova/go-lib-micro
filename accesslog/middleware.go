// Copyright 2023 Northern.tech AS
//
//	Licensed under the Apache License, Version 2.0 (the "License");
//	you may not use this file except in compliance with the License.
//	You may obtain a copy of the License at
//
//	    http://www.apache.org/licenses/LICENSE-2.0
//
//	Unless required by applicable law or agreed to in writing, software
//	distributed under the License is distributed on an "AS IS" BASIS,
//	WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//	See the License for the specific language governing permissions and
//	limitations under the License.
package accesslog

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
	"time"

	"github.com/ant0ine/go-json-rest/rest"
	"github.com/sirupsen/logrus"

	"github.com/mendersoftware/go-lib-micro/requestlog"
)

type AccessLogFormat string

const (
	DefaultLogFormat = "%t %S\033[0m \033[36;1m%Dμs\033[0m \"%r\" \033[1;30m%u \"%{User-Agent}i\"\033[0m"
	SimpleLogFormat  = "%s %Dμs %r %u %{User-Agent}i"

	TypeHTTP = "http"
)

// AccesLogMiddleware is a customized version of the AccessLogApacheMiddleware.
// It uses the request-specific custom logger (created by requestlog), which appends the Mender-specific request context.
type AccessLogMiddleware struct {
	Format       AccessLogFormat
	textTemplate *template.Template

	recorder *rest.RecorderMiddleware
}

func (mw *AccessLogMiddleware) LogFunc(startTime time.Time, w rest.ResponseWriter, r *rest.Request) {
	util := &accessLogUtil{w, r}
	fields := logrus.Fields{
		"type":   r.Proto,
		"ts":     startTime,
		"method": r.Method,
		"path":   r.URL.Path,
		"qs":     r.URL.RawQuery,
	}

	if panic := recover(); panic != nil {
		var (
			trace     [MaxTraceback]uintptr
			traceback strings.Builder
		)
		n := runtime.Callers(3, trace[:])
		frames := runtime.CallersFrames(trace[:n])
		for frame, _ := frames.Next(); frame.PC != 0 && n >= 0; frame, _ = frames.Next() {
			funcName := frame.Function
			if funcName == "" {
				fmt.Fprint(&traceback, "???\n")
			} else {
				fmt.Fprintf(&traceback, "%s(%s):%d\n",
					filepath.Base(frame.File), frame.Function, frame.Line,
				)
			}
			n--
		}
		rest.Error(w, "Internal Server Error", http.StatusInternalServerError)
		fields["panic"] = panic
		fields["trace"] = traceback.String()
	}
	rspTime := time.Since(startTime)
	// We do not need more than 3 digit fraction
	if rspTime > time.Second {
		rspTime = rspTime.Round(time.Millisecond)
	} else if rspTime > time.Millisecond {
		rspTime = rspTime.Round(time.Microsecond)
	}
	fields["responsetime"] = rspTime.String()
	fields["byteswritten"] = util.BytesWritten()
	statusCode := util.StatusCode()
	fields["status"] = statusCode

	logger := requestlog.GetRequestLogger(r)
	var level logrus.Level = logrus.InfoLevel
	if statusCode >= 500 {
		level = logrus.ErrorLevel
	} else if statusCode >= 300 {
		level = logrus.WarnLevel
	}
	logger.WithFields(fields).
		Log(level, mw.executeTextTemplate(util))
}

// MiddlewareFunc makes AccessLogMiddleware implement the Middleware interface.
func (mw *AccessLogMiddleware) MiddlewareFunc(h rest.HandlerFunc) rest.HandlerFunc {
	if mw.Format == "" {
		mw.Format = DefaultLogFormat
	}

	mw.convertFormat()

	// This middleware depends on RecorderMiddleware to work
	mw.recorder = new(rest.RecorderMiddleware)
	return mw.recorder.MiddlewareFunc(func(w rest.ResponseWriter, r *rest.Request) {
		startTime := time.Now()
		defer mw.LogFunc(startTime, w, r)
		// call the handler
		h(w, r)
	})
}

var apacheAdapter = strings.NewReplacer(
	"%b", "{{.BytesWritten | dashIf0}}",
	"%B", "{{.BytesWritten}}",
	"%D", "{{.ResponseTime | microseconds}}",
	"%h", "{{.ApacheRemoteAddr}}",
	"%H", "{{.R.Proto}}",
	"%l", "-",
	"%m", "{{.R.Method}}",
	"%P", "{{.Pid}}",
	"%q", "{{.ApacheQueryString}}",
	"%r", "{{.R.Method}} {{.R.URL.RequestURI}} {{.R.Proto}}",
	"%s", "{{.StatusCode}}",
	"%S", "\033[{{.StatusCode | statusCodeColor}}m{{.StatusCode}}",
	"%t", "{{if .StartTime}}{{.StartTime.Format \"02/Jan/2006:15:04:05 -0700\"}}{{end}}",
	"%T", "{{if .ResponseTime}}{{.ResponseTime.Seconds | printf \"%.3f\"}}{{end}}",
	"%u", "{{.RemoteUser | dashIfEmptyStr}}",
	"%{User-Agent}i", "{{.R.UserAgent | dashIfEmptyStr}}",
	"%{Referer}i", "{{.R.Referer | dashIfEmptyStr}}",
)

// Execute the text template with the data derived from the request, and return a string.
func (mw *AccessLogMiddleware) executeTextTemplate(util *accessLogUtil) string {
	buf := bytes.NewBufferString("")
	err := mw.textTemplate.Execute(buf, util)
	if err != nil {
		panic(err)
	}
	return buf.String()
}

func (mw *AccessLogMiddleware) convertFormat() {

	tmplText := apacheAdapter.Replace(string(mw.Format))

	funcMap := template.FuncMap{
		"dashIfEmptyStr": func(value string) string {
			if value == "" {
				return "-"
			}
			return value
		},
		"dashIf0": func(value int64) string {
			if value == 0 {
				return "-"
			}
			return fmt.Sprintf("%d", value)
		},
		"microseconds": func(dur *time.Duration) string {
			if dur != nil {
				return fmt.Sprintf("%d", dur.Nanoseconds()/1000)
			}
			return ""
		},
		"statusCodeColor": func(statusCode int) string {
			if statusCode >= 400 && statusCode < 500 {
				return "1;33"
			} else if statusCode >= 500 {
				return "0;31"
			}
			return "0;32"
		},
	}

	var err error
	mw.textTemplate, err = template.New("accessLog").Funcs(funcMap).Parse(tmplText)
	if err != nil {
		panic(err)
	}
}

// accessLogUtil provides a collection of utility functions that devrive data from the Request object.
// This object is used to provide data to the Apache Style template and the the JSON log record.
type accessLogUtil struct {
	W rest.ResponseWriter
	R *rest.Request
}

// As stored by the auth middlewares.
func (u *accessLogUtil) RemoteUser() string {
	if u.R.Env["REMOTE_USER"] != nil {
		return u.R.Env["REMOTE_USER"].(string)
	}
	return ""
}

// If qs exists then return it with a leadin "?", apache log style.
func (u *accessLogUtil) ApacheQueryString() string {
	if u.R.URL.RawQuery != "" {
		return "?" + u.R.URL.RawQuery
	}
	return ""
}

// When the request entered the timer middleware.
func (u *accessLogUtil) StartTime() *time.Time {
	if u.R.Env["START_TIME"] != nil {
		return u.R.Env["START_TIME"].(*time.Time)
	}
	return nil
}

// If remoteAddr is set then return is without the port number, apache log style.
func (u *accessLogUtil) ApacheRemoteAddr() string {
	remoteAddr := u.R.RemoteAddr
	if remoteAddr != "" {
		if ip, _, err := net.SplitHostPort(remoteAddr); err == nil {
			return ip
		}
	}
	return ""
}

// As recorded by the recorder middleware.
func (u *accessLogUtil) StatusCode() int {
	if u.R.Env["STATUS_CODE"] != nil {
		return u.R.Env["STATUS_CODE"].(int)
	}
	return 0
}

// As mesured by the timer middleware.
func (u *accessLogUtil) ResponseTime() *time.Duration {
	if u.R.Env["ELAPSED_TIME"] != nil {
		return u.R.Env["ELAPSED_TIME"].(*time.Duration)
	}
	return nil
}

// Process id.
func (u *accessLogUtil) Pid() int {
	return os.Getpid()
}

// As recorded by the recorder middleware.
func (u *accessLogUtil) BytesWritten() int64 {
	if u.R.Env["BYTES_WRITTEN"] != nil {
		return u.R.Env["BYTES_WRITTEN"].(int64)
	}
	return 0
}
