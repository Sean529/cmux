package local

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
)

// RPCRequest is an incoming JSON-RPC request from a client.
type RPCRequest struct {
	ID     any            `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

// RPCError describes a structured error returned by an RPC method.
type RPCError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// RPCResponse is a JSON-RPC response sent back to the client.
type RPCResponse struct {
	ID     any       `json:"id,omitempty"`
	OK     bool      `json:"ok"`
	Result any       `json:"result,omitempty"`
	Error  *RPCError `json:"error,omitempty"`
}

// RPCEvent is a server-pushed event (not tied to a request ID).
type RPCEvent struct {
	Event      string `json:"event"`
	SessionID  string `json:"session_id,omitempty"`
	PaneID     string `json:"pane_id,omitempty"`
	DataBase64 string `json:"data_base64,omitempty"`
	ReplayDone bool   `json:"replay_done,omitempty"`
	Error      string `json:"error,omitempty"`
}

// FrameWriter writes newline-delimited JSON frames to a writer, thread-safe.
type FrameWriter struct {
	mu     sync.Mutex
	writer *bufio.Writer
}

// NewFrameWriter wraps a writer with buffered, mutex-protected JSON framing.
func NewFrameWriter(w io.Writer) *FrameWriter {
	return &FrameWriter{
		writer: bufio.NewWriter(w),
	}
}

// WriteResponse marshals and writes a response frame.
func (fw *FrameWriter) WriteResponse(resp RPCResponse) error {
	return fw.writeJSONFrame(resp)
}

// WriteEvent marshals and writes an event frame.
func (fw *FrameWriter) WriteEvent(event RPCEvent) error {
	return fw.writeJSONFrame(event)
}

func (fw *FrameWriter) writeJSONFrame(payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if _, err := fw.writer.Write(data); err != nil {
		return err
	}
	if err := fw.writer.WriteByte('\n'); err != nil {
		return err
	}
	return fw.writer.Flush()
}

const maxRPCFrameBytes = 4 * 1024 * 1024

// ReadRPCFrame reads a single newline-terminated JSON frame.
// Returns the raw bytes, whether the frame was oversized, and any read error.
func ReadRPCFrame(reader *bufio.Reader) ([]byte, bool, error) {
	frame := make([]byte, 0, 1024)
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(chunk) > 0 {
			if len(frame)+len(chunk) > maxRPCFrameBytes {
				if errors.Is(err, bufio.ErrBufferFull) {
					if drainErr := discardUntilNewline(reader); drainErr != nil && !errors.Is(drainErr, io.EOF) {
						return nil, false, drainErr
					}
				}
				return nil, true, nil
			}
			frame = append(frame, chunk...)
		}

		if err == nil {
			return frame, false, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(frame) == 0 {
				return nil, false, io.EOF
			}
			return frame, false, nil
		}
		return nil, false, err
	}
}

func discardUntilNewline(reader *bufio.Reader) error {
	for {
		_, err := reader.ReadSlice('\n')
		if err == nil || errors.Is(err, io.EOF) {
			return err
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return err
	}
}

// ErrorResponse builds an error RPCResponse for the given request ID.
func ErrorResponse(id any, code, message string) RPCResponse {
	return RPCResponse{
		ID: id,
		OK: false,
		Error: &RPCError{
			Code:    code,
			Message: message,
		},
	}
}

// OKResponse builds a success RPCResponse for the given request ID.
func OKResponse(id any, result any) RPCResponse {
	return RPCResponse{
		ID:     id,
		OK:     true,
		Result: result,
	}
}

// GetStringParam extracts a string from an RPC params map.
func GetStringParam(params map[string]any, key string) (string, bool) {
	if params == nil {
		return "", false
	}
	raw, ok := params[key]
	if !ok || raw == nil {
		return "", false
	}
	value, ok := raw.(string)
	return value, ok
}

// GetIntParam extracts an integer from an RPC params map,
// handling the various numeric types JSON can decode into.
func GetIntParam(params map[string]any, key string) (int, bool) {
	if params == nil {
		return 0, false
	}
	raw, ok := params[key]
	if !ok || raw == nil {
		return 0, false
	}
	switch value := raw.(type) {
	case int:
		return value, true
	case float64:
		if math.Trunc(value) != value {
			return 0, false
		}
		return int(value), true
	case json.Number:
		n, err := value.Int64()
		if err != nil {
			return 0, false
		}
		return int(n), true
	default:
		return 0, false
	}
}

// GetMapParam extracts a map[string]any from an RPC params map.
func GetMapParam(params map[string]any, key string) (map[string]any, bool) {
	if params == nil {
		return nil, false
	}
	raw, ok := params[key]
	if !ok || raw == nil {
		return nil, false
	}
	value, ok := raw.(map[string]any)
	return value, ok
}

// GetStringMapParam extracts a map[string]string from an RPC params map.
// JSON decodes maps as map[string]any, so this converts values to strings.
func GetStringMapParam(params map[string]any, key string) (map[string]string, bool) {
	raw, ok := GetMapParam(params, key)
	if !ok {
		return nil, false
	}
	result := make(map[string]string, len(raw))
	for k, v := range raw {
		s, ok := v.(string)
		if !ok {
			return nil, false
		}
		result[k] = s
	}
	return result, true
}

// FormatError formats an error code and message for CLI display.
func FormatError(code, message string) string {
	return fmt.Sprintf("[%s] %s", code, message)
}
