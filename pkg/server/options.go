// bricolage - a content management system
// Copyright (c) 2023 Michael D Henderson
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package server

import (
	"fmt"
	"net"
	"path/filepath"
)

type Options []Option
type Option func(*Server) error

func WithHost(host string) Option {
	return func(s *Server) error {
		s.host = host
		s.hostPort = net.JoinHostPort(s.host, fmt.Sprintf("%d", s.port))
		return nil
	}
}

func WithPort(port int) Option {
	return func(s *Server) error {
		s.port = port
		s.hostPort = net.JoinHostPort(s.host, fmt.Sprintf("%d", s.port))
		return nil
	}
}

func WithRoot(path string) Option {
	return func(s *Server) error {
		s.root = path
		return nil
	}
}

func WithTemplates(path string) Option {
	return func(s *Server) error {
		if s.root == "" {
			return fmt.Errorf("must set root before templates")
		}
		s.templates = filepath.Join(s.root, path)
		return nil
	}
}
