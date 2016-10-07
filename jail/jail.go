package jail

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/eapache/go-resiliency/semaphore"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/robertkrimen/otto"
	"github.com/status-im/status-go/geth"
)

const (
	JailedRuntimeRequestTimeout = time.Second * 60
)

var (
	ErrInvalidJail = errors.New("jail environment is not properly initialized")
)

type Jail struct {
	client       *rpc.ClientRestartWrapper // lazy inited on the first call to jail.ClientRestartWrapper()
	cells        map[string]*JailedRuntime // jail supports running many isolated instances of jailed runtime
	statusJS     string
	requestQueue *geth.JailedRequestQueue
}

type JailedRuntime struct {
	id  string
	vm  *otto.Otto
	sem *semaphore.Semaphore
}

var jailInstance *Jail
var once sync.Once

func New() *Jail {
	once.Do(func() {
		jailInstance = &Jail{
			cells: make(map[string]*JailedRuntime),
		}
	})

	return jailInstance
}

func Init(js string) *Jail {
	jailInstance = New() // singleton, we will always get the same reference
	jailInstance.statusJS = js

	return jailInstance
}

func GetInstance() *Jail {
	return New() // singleton, we will always get the same reference
}

func NewJailedRuntime(id string) *JailedRuntime {
	return &JailedRuntime{
		id:  id,
		vm:  otto.New(),
		sem: semaphore.New(1, JailedRuntimeRequestTimeout),
	}
}

func (jail *Jail) Parse(chatId string, js string) string {
	if jail == nil {
		return printError(ErrInvalidJail.Error())
	}

	jail.cells[chatId] = NewJailedRuntime(chatId)
	vm := jail.cells[chatId].vm

	initJjs := jail.statusJS + ";"
	_, err := vm.Run(initJjs)
	vm.Set("jeth", struct{}{})

	jethObj, _ := vm.Get("jeth")
	jethObj.Object().Set("send", jail.Send)
	jethObj.Object().Set("sendAsync", jail.Send)

	jjs := Web3_JS + `
	var Web3 = require('web3');
	var web3 = new Web3(jeth);
	var Bignumber = require("bignumber.js");
        function bn(val){
            return new Bignumber(val);
        }
	` + js + "; var catalog = JSON.stringify(_status_catalog);"
	vm.Run(jjs)

	res, _ := vm.Get("catalog")

	return printResult(res.String(), err)
}

func (jail *Jail) Call(chatId string, path string, args string) string {
	_, err := jail.ClientRestartWrapper()
	if err != nil {
		return printError(err.Error())
	}

	cell, ok := jail.cells[chatId]
	if !ok {
		return printError(fmt.Sprintf("Cell[%s] doesn't exist.", chatId))
	}

	// serialize requests to VM
	cell.sem.Acquire()
	defer cell.sem.Release()

	res, err := cell.vm.Call("call", nil, path, args)

	return printResult(res.String(), err)
}

func (jail *Jail) GetVM(chatId string) (*otto.Otto, error) {
	if jail == nil {
		return nil, ErrInvalidJail
	}

	cell, ok := jail.cells[chatId]
	if !ok {
		return nil, fmt.Errorf("Cell[%s] doesn't exist.", chatId)
	}

	return cell.vm, nil
}

// Send will serialize the first argument, send it to the node and returns the response.
func (jail *Jail) Send(call otto.FunctionCall) (response otto.Value) {
	clientFactory, err := jail.ClientRestartWrapper()
	if err != nil {
		return newErrorResponse(call, -32603, err.Error(), nil)
	}

	requestQueue, err := jail.RequestQueue()
	if err != nil {
		return newErrorResponse(call, -32603, err.Error(), nil)
	}

	// Remarshal the request into a Go value.
	JSON, _ := call.Otto.Object("JSON")
	reqVal, err := JSON.Call("stringify", call.Argument(0))
	if err != nil {
		throwJSException(err.Error())
	}
	var (
		rawReq = []byte(reqVal.String())
		reqs   []geth.RPCCall
		batch  bool
	)
	if rawReq[0] == '[' {
		batch = true
		json.Unmarshal(rawReq, &reqs)
	} else {
		batch = false
		reqs = make([]geth.RPCCall, 1)
		json.Unmarshal(rawReq, &reqs[0])
	}

	// Execute the requests.
	resps, _ := call.Otto.Object("new Array()")
	for _, req := range reqs {
		resp, _ := call.Otto.Object(`({"jsonrpc":"2.0"})`)
		resp.Set("id", req.Id)
		var result json.RawMessage

		// do extra request pre and post processing (message id persisting, setting tx context)
		requestQueue.PreProcessRequest(call.Otto, req)
		defer requestQueue.PostProcessRequest(call.Otto, req)

		client := clientFactory.Client()
		errc := make(chan error, 1)
		errc2 := make(chan error)
		go func() {
			errc2 <- <-errc
		}()
		errc <- client.Call(&result, req.Method, req.Params...)
		err = <-errc2

		switch err := err.(type) {
		case nil:
			if result == nil {
				// Special case null because it is decoded as an empty
				// raw message for some reason.
				resp.Set("result", otto.NullValue())
			} else {
				resultVal, err := JSON.Call("parse", string(result))
				if err != nil {
					resp = newErrorResponse(call, -32603, err.Error(), &req.Id).Object()
				} else {
					resp.Set("result", resultVal)
				}
			}
		case rpc.Error:
			resp.Set("error", map[string]interface{}{
				"code":    err.ErrorCode(),
				"message": err.Error(),
			})
		default:
			resp = newErrorResponse(call, -32603, err.Error(), &req.Id).Object()
		}
		resps.Call("push", resp)
	}

	// Return the responses either to the callback (if supplied)
	// or directly as the return value.
	if batch {
		response = resps.Value()
	} else {
		response, _ = resps.Get("0")
	}
	if fn := call.Argument(1); fn.Class() == "Function" {
		fn.Call(otto.NullValue(), otto.NullValue(), response)
		return otto.UndefinedValue()
	}
	return response
}

func (jail *Jail) ClientRestartWrapper() (*rpc.ClientRestartWrapper, error) {
	if jail == nil {
		return nil, ErrInvalidJail
	}

	if jail.client != nil {
		return jail.client, nil
	}

	nodeManager := geth.GetNodeManager()
	if !nodeManager.HasNode() {
		return nil, geth.ErrInvalidGethNode
	}

	// obtain RPC client from running node
	client, err := nodeManager.ClientRestartWrapper()
	if err != nil {
		return nil, err
	}
	jail.client = client

	return jail.client, nil
}

func (jail *Jail) RequestQueue() (*geth.JailedRequestQueue, error) {
	if jail == nil {
		return nil, ErrInvalidJail
	}

	if jail.requestQueue != nil {
		return jail.requestQueue, nil
	}

	nodeManager := geth.GetNodeManager()
	if !nodeManager.HasNode() {
		return nil, geth.ErrInvalidGethNode
	}

	requestQueue, err := nodeManager.JailedRequestQueue()
	if err != nil {
		return nil, err
	}
	jail.requestQueue = requestQueue

	return jail.requestQueue, nil
}

func newErrorResponse(call otto.FunctionCall, code int, msg string, id interface{}) otto.Value {
	// Bundle the error into a JSON RPC call response
	m := map[string]interface{}{"version": "2.0", "id": id, "error": map[string]interface{}{"code": code, msg: msg}}
	res, _ := json.Marshal(m)
	val, _ := call.Otto.Run("(" + string(res) + ")")
	return val
}

// throwJSException panics on an otto.Value. The Otto VM will recover from the
// Go panic and throw msg as a JavaScript error.
func throwJSException(msg interface{}) otto.Value {
	val, err := otto.ToValue(msg)
	if err != nil {
		glog.V(logger.Error).Infof("Failed to serialize JavaScript exception %v: %v", msg, err)
	}
	panic(val)
}

func printError(error string) string {
	str := geth.JSONError{
		Error: error,
	}
	outBytes, _ := json.Marshal(&str)
	return string(outBytes)
}

func printResult(res string, err error) string {
	var out string
	if err != nil {
		out = printError(err.Error())
	} else {
		if "undefined" == res {
			res = "null"
		}
		out = fmt.Sprintf(`{"result": %s}`, res)
	}

	return out
}
