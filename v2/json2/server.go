// Copyright 2009 The Go Authors. All rights reserved.
// Copyright 2012 The Gorilla Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package json2

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gorilla/rpc/v2"
	"github.com/tidwall/gjson"
	"io"
	"io/ioutil"
	"net/http"
)

var null = json.RawMessage([]byte("null"))
var Version = "2.0"

type JSONEncoder interface {
	Encode(v interface{}) error
}

// ----------------------------------------------------------------------------
// Request and Response
// ----------------------------------------------------------------------------

// serverRequest represents a JSON-RPC request received by the server.
type serverRequest struct {
	// JSON-RPC protocol.
	Version string `json:"jsonrpc"`

	// A String containing the name of the method to be invoked.
	Method string `json:"method"`

	// A Structured value to pass as arguments to the method.
	Params *json.RawMessage `json:"params"`

	// The request id. MUST be a string, number or null.
	// Our implementation will not do type checking for id.
	// It will be copied as it is.
	Id *json.RawMessage `json:"id"`
}

// serverResponse represents a JSON-RPC response returned by the server.
type serverResponse struct {
	// JSON-RPC protocol.
	Version string `json:"jsonrpc"`

	// The Object that was returned by the invoked method. This must be null
	// in case there was an error invoking the method.
	// As per spec the member will be omitted if there was an error.
	Result interface{} `json:"result,omitempty"`

	// An Error object if there was an error invoking the method. It must be
	// null if there was no error.
	// As per spec the member will be omitted if there was no error.
	Error *Error `json:"error,omitempty"`

	// This must be the same id as the request it is responding to.
	Id *json.RawMessage `json:"id"`
}

// ----------------------------------------------------------------------------
// Codec
// ----------------------------------------------------------------------------

type options struct {
	encoderSelector    rpc.EncoderSelector
	jsonEncoderFactory func(w io.Writer) JSONEncoder
	errorMapper        func(context.Context, error) error
	mapAllErrors       bool
}

type Option interface {
	apply(opts *options)
}

type optionFunc func(opts *options)

func (f optionFunc) apply(opts *options) {
	f(opts)
}

func WithEncoderSelector(encSel rpc.EncoderSelector) Option {
	return optionFunc(func(opts *options) { opts.encoderSelector = encSel })
}

// WithErrorMapper defines an `errorMapper` function that will be called if the Service implementation
// returns an error, with that error as a param, replacing it by the value returned by this function.
// This function is intended to decouple your service implementation from the codec itself, making
// possible to return abstract errors in your service, and then mapping them here to the JSON-RPC
// error codes.
func WithErrorMapper(mapper func(context.Context, error) error) Option {
	return optionFunc(func(opts *options) { opts.errorMapper = mapper })
}

func MapAllErrors() Option {
	return optionFunc(func(opts *options) { opts.mapAllErrors = true })
}

func WithJSONEncoderFactory(factory func(w io.Writer) JSONEncoder) Option {
	return optionFunc(func(opts *options) { opts.jsonEncoderFactory = factory })
}

// NewCustomCodec returns a new JSON Codec based on passed encoder selector.
func NewCustomCodec(opts ...Option) *Codec {
	codec := &Codec{
		options: options{
			encoderSelector:    rpc.DefaultEncoderSelector,
			jsonEncoderFactory: builtInJSONEncoderFactory,
		},
	}

	for _, opt := range opts {
		opt.apply(&codec.options)
	}

	return codec
}

func builtInJSONEncoderFactory(w io.Writer) JSONEncoder {
	return json.NewEncoder(w)
}

// NewCodec returns a new JSON Codec.
func NewCodec() *Codec {
	return NewCustomCodec()
}

// Codec creates a CodecRequest to process each request.
type Codec struct {
	options
}

// NewRequest returns a CodecRequest.
func (c *Codec) NewRequest(r *http.Request) rpc.CodecRequest {
	return newCodecRequest(
		r,
		c.encoderSelector.Select(r),
		c.jsonEncoderFactory,
		c.errorMapper,
		c.mapAllErrors,
	)
}

// ----------------------------------------------------------------------------
// CodecRequest
// ----------------------------------------------------------------------------

// newCodecRequest returns a new CodecRequest.
func newCodecRequest(
	r *http.Request,
	encoder rpc.Encoder,
	jsonEncoderFactory func(w io.Writer) JSONEncoder,
	errorMapper func(context.Context, error) error,
	mapAllErrors bool,
) rpc.CodecRequest {

	requests, isBatch, err := parseMessage(r)
	if err != nil {
		err = &Error{
			Code:    E_PARSE,
			Message: err.Error(),
		}
		requests = []*serverRequest{{}} //dump requests to make sure that method will called
	} else {
		for _, req := range requests {
			if req.Version != Version {
				err = &Error{
					Code:    E_INVALID_REQ,
					Message: "jsonrpc must be " + Version,
					Data:    req,
				}
				break
			}
		}
	}

	return &CodecRequest{
		requests:           requests,
		isBatch:            isBatch,
		err:                err,
		encoder:            encoder,
		jsonEncoderFactory: jsonEncoderFactory,
		errorMapper:        errorMapper,
		mapAllErrors:       mapAllErrors,
	}
}

// IsBatch returns true when the first non-whitespace characters is '['
func IsBatch(raw json.RawMessage) bool {
	return gjson.ParseBytes(raw).IsArray()
} // CodecRequest decodes and encodes a single request.

func parseMessage(r *http.Request) ([]*serverRequest, bool, error) {
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, false, fmt.Errorf("reading request body: %w", err)
	}
	r.Body.Close()

	raw := json.RawMessage(body)
	if !IsBatch(raw) {
		msgs := []*serverRequest{{}}
		if err := json.Unmarshal(raw, &msgs[0]); err != nil {
			return nil, false, fmt.Errorf("json unmarshal single request error: %v", err)
		}
		return msgs, false, nil
	}

	var msgs []*serverRequest
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return nil, false, fmt.Errorf("json unmarshal batch request error: %v", err)
	}
	return msgs, true, nil
}

// CodecRequest decodes and encodes a single request.
type CodecRequest struct {
	requests           []*serverRequest
	isBatch            bool
	err                error
	encoder            rpc.Encoder
	jsonEncoderFactory func(w io.Writer) JSONEncoder
	errorMapper        func(context.Context, error) error
	mapAllErrors       bool
	batchResponses     []*serverResponse
}

func (c *CodecRequest) RequestCount() int {
	return len(c.requests)
}

// Method returns the RPC method for the current request.
//
// The method uses a dotted notation as in "Service.Method".
func (c *CodecRequest) Method(reqIdx int) (string, error) {
	if c.err == nil {
		return c.requests[reqIdx].Method, nil
	}
	return "", c.err
}

// ReadRequest fills the request object for the RPC method.
//
// ReadRequest parses request parameters in two supported forms in
// accordance with http://www.jsonrpc.org/specification#parameter_structures
//
// by-position: params MUST be an Array, containing the
// values in the Server expected order.
//
// by-name: params MUST be an Object, with member names
// that match the Server expected parameter names. The
// absence of expected names MAY result in an error being
// generated. The names MUST match exactly, including
// case, to the method's expected parameters.
func (c *CodecRequest) ReadRequest(reqIdx int, args interface{}) error {
	request := c.requests[reqIdx]
	if c.err == nil && request.Params != nil {
		// Note: if c.request.Params is nil it's not an error, it's an optional member.
		// JSON params structured object. Unmarshal to the args object.
		if err := json.Unmarshal(*request.Params, args); err != nil {
			// Clearly JSON params is not a structured object, let's try to
			// turn the struct into a slice of its fields and parse again. This is
			// to handle array params but re-mapped into the struct fields.
			params, err := structFieldsToFieldsSlice(args)
			if err != nil {
				return fmt.Errorf("transforming struct fields to array of fields: %w", err)
			}

			if err = json.Unmarshal(*request.Params, &params); err != nil {
				// Clearly JSON params is not a structured object, and
				// reducing fields to a single array did not work.
				// Final fallback and attempt an unmarshal with JSON params as
				// array value and RPC params is struct. Unmarshal into
				// array containing the request struct.
				params := [1]interface{}{args}

				if err = json.Unmarshal(*request.Params, &params); err != nil {
					c.err = &Error{
						Code:    E_INVALID_REQ,
						Message: err.Error(),
						Data:    request.Params,
					}
				}
			}
		}
	}
	return c.err
}

// WriteResponse encodes the response and writes it to the ResponseWriter.
func (c *CodecRequest) WriteResponse(reqIdx int, w http.ResponseWriter, reply interface{}) {
	res := &serverResponse{
		Version: Version,
		Result:  reply,
		Id:      c.requests[reqIdx].Id,
	}
	c.writeServerResponse(reqIdx, w, res)
}

func (c *CodecRequest) WriteError(ctx context.Context, reqIdx int, w http.ResponseWriter, status int, err error) {
	err = c.tryToMapIfNotAnErrorAlready(ctx, err)
	jsonErr, ok := err.(*Error)
	if !ok {
		jsonErr = &Error{
			Code:    E_SERVER,
			Message: err.Error(),
		}
	}
	res := &serverResponse{
		Version: Version,
		Error:   jsonErr,
		Id:      c.requests[reqIdx].Id,
	}
	c.writeServerResponse(reqIdx, w, res)
}

func (c CodecRequest) tryToMapIfNotAnErrorAlready(ctx context.Context, err error) error {
	if c.errorMapper == nil {
		return err
	}

	if c.mapAllErrors {
		return c.errorMapper(ctx, err)
	}

	if _, ok := err.(*Error); ok {
		return err
	}

	return c.errorMapper(ctx, err)
}

func (c *CodecRequest) writeServerResponse(reqIdx int, w http.ResponseWriter, res *serverResponse) {
	var out interface{} = res
	if c.isBatch {
		c.batchResponses = append(c.batchResponses, res)
		batchCompleted := reqIdx == len(c.requests)-1
		if !batchCompleted {
			return
		}
		out = c.batchResponses
	}

	// Id is null for notifications and they don't have a response, unless we couldn't even parse the JSON, in that
	// case we can't know whether it was intended to be a notification
	if c.requests[reqIdx].Id != nil || isParseErrorResponse(res) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		encoder := c.jsonEncoderFactory(c.encoder.Encode(w))
		err := encoder.Encode(out)

		// Not sure in which case will this happen. But seems harmless.
		if err != nil {
			rpc.WriteError(w, http.StatusInternalServerError, err.Error())
		}
	}
}

func isParseErrorResponse(res *serverResponse) bool {
	return res != nil && res.Error != nil && res.Error.Code == E_PARSE
}

type EmptyResponse struct {
}

// DecodeClientResponse decodes the response body of a client request into
// the interface reply.
func DecodeClientResponse(r io.Reader) ([]*clientResponse, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	raw := json.RawMessage(data)
	c := &clientResponse{}
	if !IsBatch(raw) {
		err = json.Unmarshal(data, &c)
		if err != nil {
			return nil, fmt.Errorf("decoding none batch response body: %w", err)
		}

		return []*clientResponse{c}, nil
	}

	var cr []*clientResponse
	err = json.Unmarshal(data, &cr)
	if err != nil {
		return nil, fmt.Errorf("decoding batch response body: %w", err)
	}

	return cr, nil
}
