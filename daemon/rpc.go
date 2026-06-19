//go:build windows

package daemon

import (
	"encoding/json"
	"fmt"
	"time"
)

type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      interface{}     `json:"id"`
}

type RPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	Result  interface{} `json:"result,omitempty"`
	Error   *RPCError   `json:"error,omitempty"`
	ID      interface{} `json:"id"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type nameParam struct {
	Name string `json:"name"`
}

type messageParam struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
	From   string `json:"from,omitempty"`
}

type registerParam struct {
	Name        string   `json:"name"`
	Command     string   `json:"command"`
	Args        []string `json:"args,omitempty"`
	Interval    string   `json:"interval,omitempty"`
	MaxRestarts int      `json:"max_restarts,omitempty"`
	Charge      string   `json:"charge,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

func (d *Daemon) HandleRPC(req RPCRequest) RPCResponse {
	switch req.Method {
	case "list":
		return rpcOK(req.ID, d.List())

	case "status":
		var p nameParam
		if err := parseParams(req.Params, &p); err != nil {
			return rpcErr(req.ID, -32602, err.Error())
		}
		s, err := d.StatusOf(p.Name)
		if err != nil {
			return rpcErr(req.ID, -32000, err.Error())
		}
		return rpcOK(req.ID, s)

	case "start":
		var p nameParam
		if err := parseParams(req.Params, &p); err != nil {
			return rpcErr(req.ID, -32602, err.Error())
		}
		if err := d.StartAngl(p.Name); err != nil {
			return rpcErr(req.ID, -32000, err.Error())
		}
		return rpcOK(req.ID, "ok")

	case "stop":
		var p nameParam
		if err := parseParams(req.Params, &p); err != nil {
			return rpcErr(req.ID, -32602, err.Error())
		}
		if err := d.StopAngl(p.Name); err != nil {
			return rpcErr(req.ID, -32000, err.Error())
		}
		return rpcOK(req.ID, "ok")

	case "restart":
		var p nameParam
		if err := parseParams(req.Params, &p); err != nil {
			return rpcErr(req.ID, -32602, err.Error())
		}
		if err := d.RestartAngl(p.Name); err != nil {
			return rpcErr(req.ID, -32000, err.Error())
		}
		return rpcOK(req.ID, "ok")

	case "reload":
		result, err := d.Reload()
		if err != nil {
			return rpcErr(req.ID, -32000, err.Error())
		}
		return rpcOK(req.ID, result)

	case "enable":
		var p nameParam
		if err := parseParams(req.Params, &p); err != nil {
			return rpcErr(req.ID, -32602, err.Error())
		}
		if err := d.Enable(p.Name); err != nil {
			return rpcErr(req.ID, -32000, err.Error())
		}
		return rpcOK(req.ID, "ok")

	case "disable":
		var p nameParam
		if err := parseParams(req.Params, &p); err != nil {
			return rpcErr(req.ID, -32602, err.Error())
		}
		if err := d.Disable(p.Name); err != nil {
			return rpcErr(req.ID, -32000, err.Error())
		}
		return rpcOK(req.ID, "ok")

	case "message":
		var p messageParam
		if err := parseParams(req.Params, &p); err != nil {
			return rpcErr(req.ID, -32602, err.Error())
		}
		data, err := d.Message(p.Name, p.Prompt, p.From)
		if err != nil {
			return rpcErr(req.ID, -32000, err.Error())
		}
		return rpcOK(req.ID, data)

	case "register":
		var p registerParam
		if err := parseParams(req.Params, &p); err != nil {
			return rpcErr(req.ID, -32602, err.Error())
		}
		def := AnglDef{
			Command:     p.Command,
			Args:        p.Args,
			Interval:    p.Interval,
			MaxRestarts: p.MaxRestarts,
			Charge:      p.Charge,
			Tags:        p.Tags,
			CreatedAt:   time.Now().Format(time.RFC3339),
		}
		if err := d.Register(p.Name, def); err != nil {
			return rpcErr(req.ID, -32000, err.Error())
		}
		return rpcOK(req.ID, "registered")

	case "unregister":
		var p nameParam
		if err := parseParams(req.Params, &p); err != nil {
			return rpcErr(req.ID, -32602, err.Error())
		}
		if err := d.Unregister(p.Name); err != nil {
			return rpcErr(req.ID, -32000, err.Error())
		}
		return rpcOK(req.ID, "unregistered")

	default:
		return rpcErr(req.ID, -32601, fmt.Sprintf("unknown method %q", req.Method))
	}
}

func parseParams(raw json.RawMessage, v interface{}) error {
	if len(raw) == 0 {
		return fmt.Errorf("params required")
	}
	return json.Unmarshal(raw, v)
}

func rpcOK(id interface{}, result interface{}) RPCResponse {
	return RPCResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func rpcErr(id interface{}, code int, msg string) RPCResponse {
	return RPCResponse{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: msg}}
}
