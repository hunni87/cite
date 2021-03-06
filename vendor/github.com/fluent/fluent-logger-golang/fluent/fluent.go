package fluent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"reflect"
	"strconv"
	"sync"
	"time"
)

const (
	defaultHost                   = "127.0.0.1"
	defaultNetwork                = "tcp"
	defaultSocketPath             = ""
	defaultPort                   = 24224
	defaultTimeout                = 3 * time.Second
	defaultBufferLimit            = 8 * 1024 * 1024
	defaultRetryWait              = 500
	defaultMaxRetry               = 13
	defaultReconnectWaitIncreRate = 1.5
)

type Config struct {
	FluentPort       int           `json:"fluent_port"`
	FluentHost       string        `json:"fluent_host"`
	FluentNetwork    string        `json:"fluent_network"`
	FluentSocketPath string        `json:"fluent_socket_path"`
	Timeout          time.Duration `json:"timeout"`
	BufferLimit      int           `json:"buffer_limit"`
	RetryWait        int           `json:"retry_wait"`
	MaxRetry         int           `json:"max_retry"`
	TagPrefix        string        `json:"tag_prefix"`
	AsyncConnect     bool          `json:"async_connect"`
	MarshalAsJSON    bool          `json:"marshal_as_json"`
}

type Fluent struct {
	Config
	conn         io.WriteCloser
	pending      []byte
	reconnecting bool
	mu           sync.Mutex
}

// New creates a new Logger.
func New(config Config) (f *Fluent, err error) {
	if config.FluentNetwork == "" {
		config.FluentNetwork = defaultNetwork
	}
	if config.FluentHost == "" {
		config.FluentHost = defaultHost
	}
	if config.FluentPort == 0 {
		config.FluentPort = defaultPort
	}
	if config.FluentSocketPath == "" {
		config.FluentSocketPath = defaultSocketPath
	}
	if config.Timeout == 0 {
		config.Timeout = defaultTimeout
	}
	if config.BufferLimit == 0 {
		config.BufferLimit = defaultBufferLimit
	}
	if config.RetryWait == 0 {
		config.RetryWait = defaultRetryWait
	}
	if config.MaxRetry == 0 {
		config.MaxRetry = defaultMaxRetry
	}
	if config.AsyncConnect {
		f = &Fluent{Config: config, reconnecting: true}
		f.reconnect()
	} else {
		f = &Fluent{Config: config, reconnecting: false}
		err = f.connect()
	}
	return
}

// Post writes the output for a logging event.
//
// Examples:
//
//  // send string
//  f.Post("tag_name", "data")
//
//  // send map[string]
//  mapStringData := map[string]string{
//  	"foo":  "bar",
//  }
//  f.Post("tag_name", mapStringData)
//
//  // send message with specified time
//  mapStringData := map[string]string{
//  	"foo":  "bar",
//  }
//  tm := time.Now()
//  f.PostWithTime("tag_name", tm, mapStringData)
//
//  // send struct
//  structData := struct {
//  		Name string `msg:"name"`
//  } {
//  		"john smith",
//  }
//  f.Post("tag_name", structData)
//
func (f *Fluent) Post(tag string, message interface{}) error {
	timeNow := time.Now()
	return f.PostWithTime(tag, timeNow, message)
}

func (f *Fluent) PostWithTime(tag string, tm time.Time, message interface{}) error {
	if len(f.TagPrefix) > 0 {
		tag = f.TagPrefix + "." + tag
	}

	msg := reflect.ValueOf(message)
	msgtype := msg.Type()

	if msgtype.Kind() == reflect.Struct {
		// message should be tagged by "codec" or "msg"
		kv := make(map[string]interface{})
		fields := msgtype.NumField()
		for i := 0; i < fields; i++ {
			field := msgtype.Field(i)
			name := field.Name
			if n1 := field.Tag.Get("msg"); n1 != "" {
				name = n1
			} else if n2 := field.Tag.Get("codec"); n2 != "" {
				name = n2
			}
			kv[name] = msg.FieldByIndex(field.Index).Interface()
		}
		return f.EncodeAndPostData(tag, tm, kv)
	}

	if msgtype.Kind() != reflect.Map {
		return errors.New("messge must be a map")
	} else if msgtype.Key().Kind() != reflect.String {
		return errors.New("map keys must be strings")
	}

	kv := make(map[string]interface{})
	for _, k := range msg.MapKeys() {
		kv[k.String()] = msg.MapIndex(k).Interface()
	}

	return f.EncodeAndPostData(tag, tm, kv)
}

func (f *Fluent) EncodeAndPostData(tag string, tm time.Time, message interface{}) error {
	if data, dumperr := f.EncodeData(tag, tm, message); dumperr != nil {
		return fmt.Errorf("fluent#EncodeAndPostData: can't convert '%s' to msgpack:%s", message, dumperr)
		// fmt.Println("fluent#Post: can't convert to msgpack:", message, dumperr)
	} else {
		f.PostRawData(data)
		return nil
	}
}

func (f *Fluent) PostRawData(data []byte) {
	f.mu.Lock()
	f.pending = append(f.pending, data...)
	f.mu.Unlock()
	if err := f.send(); err != nil {
		f.close()
		if len(f.pending) > f.Config.BufferLimit {
			f.flushBuffer()
		}
	} else {
		f.flushBuffer()
	}
}

// For sending forward protocol adopted JSON
type MessageChunk struct {
	message Message
}

// Golang default marshaler does not support
// ["value", "value2", {"key":"value"}] style marshaling.
// So, it should write JSON marshaler by hand.
func (chunk *MessageChunk) MarshalJSON() ([]byte, error) {
	data, err := json.Marshal(chunk.message.Record)
	return []byte(fmt.Sprintf("[\"%s\",%d,%s,null]", chunk.message.Tag,
		chunk.message.Time, data)), err
}

func (f *Fluent) EncodeData(tag string, tm time.Time, message interface{}) (data []byte, err error) {
	timeUnix := tm.Unix()
	if f.Config.MarshalAsJSON {
		msg := Message{Tag: tag, Time: timeUnix, Record: message}
		chunk := &MessageChunk{message: msg}
		data, err = json.Marshal(chunk)
	} else {
		msg := &Message{Tag: tag, Time: timeUnix, Record: message}
		data, err = msg.MarshalMsg(nil)
	}
	return
}

// Close closes the connection.
func (f *Fluent) Close() (err error) {
	if len(f.pending) > 0 {
		err = f.send()
	}
	f.close()
	return
}

// close closes the connection.
func (f *Fluent) close() (err error) {
	if f.conn != nil {
		f.mu.Lock()
		defer f.mu.Unlock()
	} else {
		return
	}
	if f.conn != nil {
		f.conn.Close()
		f.conn = nil
	}
	return
}

// connect establishes a new connection using the specified transport.
func (f *Fluent) connect() (err error) {
	switch f.Config.FluentNetwork {
	case "tcp":
		f.conn, err = net.DialTimeout(f.Config.FluentNetwork, f.Config.FluentHost+":"+strconv.Itoa(f.Config.FluentPort), f.Config.Timeout)
	case "unix":
		f.conn, err = net.DialTimeout(f.Config.FluentNetwork, f.Config.FluentSocketPath, f.Config.Timeout)
	default:
		err = net.UnknownNetworkError(f.Config.FluentNetwork)
	}
	return
}

func e(x, y float64) int {
	return int(math.Pow(x, y))
}

func (f *Fluent) reconnect() {
	go func() {
		for i := 0; ; i++ {
			err := f.connect()
			if err == nil {
				f.mu.Lock()
				f.reconnecting = false
				f.mu.Unlock()
				break
			} else {
				if i == f.Config.MaxRetry {
					panic("fluent#reconnect: failed to reconnect!")
				}
				waitTime := f.Config.RetryWait * e(defaultReconnectWaitIncreRate, float64(i-1))
				time.Sleep(time.Duration(waitTime) * time.Millisecond)
			}
		}
	}()
}

func (f *Fluent) flushBuffer() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = f.pending[0:0]
}

func (f *Fluent) send() (err error) {
	if f.conn == nil {
		if f.reconnecting == false {
			f.mu.Lock()
			f.reconnecting = true
			f.mu.Unlock()
			f.reconnect()
		}
		err = errors.New("fluent#send: can't send logs, client is reconnecting")
	} else {
		f.mu.Lock()
		_, err = f.conn.Write(f.pending)
		f.mu.Unlock()
	}
	return
}
