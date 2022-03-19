// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package api4

import (
	"net/http"
)

type requestHandler interface {
	execute(*Context, *http.Request, interface{}) requestHandler
	then(requestHandler) requestHandler
	getData() interface{}
}

type concreteRequestHandler struct {
	next     requestHandler
	execFunc func(*Context, *http.Request, interface{}, func(interface{}))
	data     interface{}
}

func (h *concreteRequestHandler) setData(data interface{}) {
	h.data = data
}

func (h *concreteRequestHandler) getData() interface{} {
	return h.data
}

func (h *concreteRequestHandler) execute(c *Context, r *http.Request, data interface{}) requestHandler {
	h.setData(data)
	if c.Err != nil && h.next != nil {
		if h.next != nil {
			return h.next.execute(c, r, h.data)
		}
		return h
	}
	h.execFunc(c, r, h.data, h.setData)
	if h.next != nil {
		return h.next.execute(c, r, h.data)
	}
	return h
}

func (h *concreteRequestHandler) then(next requestHandler) requestHandler {
	h.next = next
	return h
}

func newRequestHandler(execFunc func(*Context, *http.Request, interface{}, func(interface{}))) requestHandler {
	return &concreteRequestHandler{
		execFunc: execFunc,
	}
}
