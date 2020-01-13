// Code generated by MockGen. DO NOT EDIT.
// Source: src/cmd/services/m3coordinator/server/m3msg/types.go

// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

// Package m3msg is a generated GoMock package.
package m3msg

import (
	"reflect"

	"github.com/golang/mock/gomock"
)

// MockCallbackable is a mock of Callbackable interface
type MockCallbackable struct {
	ctrl     *gomock.Controller
	recorder *MockCallbackableMockRecorder
}

// MockCallbackableMockRecorder is the mock recorder for MockCallbackable
type MockCallbackableMockRecorder struct {
	mock *MockCallbackable
}

// NewMockCallbackable creates a new mock instance
func NewMockCallbackable(ctrl *gomock.Controller) *MockCallbackable {
	mock := &MockCallbackable{ctrl: ctrl}
	mock.recorder = &MockCallbackableMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use
func (m *MockCallbackable) EXPECT() *MockCallbackableMockRecorder {
	return m.recorder
}

// Callback mocks base method
func (m *MockCallbackable) Callback(t CallbackType) {
	m.ctrl.T.Helper()
	m.ctrl.Call(m, "Callback", t)
}

// Callback indicates an expected call of Callback
func (mr *MockCallbackableMockRecorder) Callback(t interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Callback", reflect.TypeOf((*MockCallbackable)(nil).Callback), t)
}
