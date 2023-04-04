/*
 * Copyright (c) 2017, MegaEase
 * All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package grpcproxy provides the proxy filter of gRPC.
package grpcproxy

import (
	stdcontext "context"
	"fmt"
	"github.com/megaease/easegress/pkg/context"
	"github.com/megaease/easegress/pkg/filters"
	"github.com/megaease/easegress/pkg/filters/proxies"
	"github.com/megaease/easegress/pkg/logger"
	"github.com/megaease/easegress/pkg/protocols/grpcprot"
	"github.com/megaease/easegress/pkg/resilience"
	"github.com/megaease/easegress/pkg/supervisor"
	"github.com/megaease/easegress/pkg/util/objectpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"time"
)

const (
	// Kind is the kind of Proxy.
	Kind = "GRPCProxy"

	resultInternalError = "internalError"
	resultClientError   = "clientError"
	resultServerError   = "serverError"

	// result for resilience
	resultShortCircuited = "shortCircuited"
)

var (
	kind = &filters.Kind{
		Name:        Kind,
		Description: "GRPCProxy sets the proxy of grpc servers",
		Results: []string{
			resultInternalError,
			resultClientError,
			resultServerError,
			resultShortCircuited,
		},
		DefaultSpec: func() filters.Spec {
			return &Spec{}
		},
		CreateInstance: func(spec filters.Spec) filters.Filter {
			return &Proxy{
				super: spec.Super(),
				spec:  spec.(*Spec),
			}
		},
	}
	defaultDialOpts = []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithCodec(&GrpcCodec{}),
		grpc.WithBlock()}
)

var _ filters.Filter = (*Proxy)(nil)
var _ filters.Resiliencer = (*Proxy)(nil)

func init() {
	filters.Register(kind)
}

type (
	// Proxy is the filter Proxy.
	Proxy struct {
		super *supervisor.Supervisor
		spec  *Spec

		mainPool       *ServerPool
		candidatePools []*ServerPool
		pool           *objectpool.MultiPool
		poolSpec       *objectpool.Spec
		timeout        time.Duration
		borrowTimeout  time.Duration
		connectTimeout time.Duration
	}

	// Spec describes the Proxy.
	Spec struct {
		filters.BaseSpec    `json:",inline"`
		Pools               []*ServerPoolSpec `json:"pools" jsonschema:"required"`
		Timeout             string            `json:"timeout" jsonschema:"omitempty,format=duration"`
		BorrowTimeout       string            `json:"borrowTimeout" jsonschema:"omitempty,format=duration"`
		ConnectTimeout      string            `json:"connectTimeout" jsonschema:"omitempty,format=duration"`
		InitConnsPerHost    int               `json:"initConnsPerHost" jsonschema:"omitempty"`
		MaxIdleConnsPerHost int               `json:"maxIdleConnsPerHost" jsonschema:"omitempty"`
	}

	// Server is the backend server.
	Server = proxies.Server
	// RequestMatcher is the interface of a request matcher
	RequestMatcher = proxies.RequestMatcher
	// LoadBalancer is the interface of a load balancer.
	LoadBalancer = proxies.LoadBalancer
	// LoadBalanceSpec is the spec of a load balancer.
	LoadBalanceSpec = proxies.LoadBalanceSpec
	// BaseServerPool is the base of a server pool.
	BaseServerPool = proxies.ServerPoolBase
	// BaseServerPoolSpec is the spec of BaseServerPool.
	BaseServerPoolSpec = proxies.ServerPoolBaseSpec
	clientConnWrapper  struct {
		*grpc.ClientConn
	}
)

func (c *clientConnWrapper) Destroy() {
	c.Close()
}

func (c *clientConnWrapper) HealthCheck() bool {
	return c.GetState() != connectivity.Shutdown
}

// Validate validates Spec.
func (s *Spec) Validate() error {
	numMainPool := 0
	for i, pool := range s.Pools {
		if pool.Filter == nil {
			numMainPool++
		}
		if err := pool.Validate(); err != nil {
			return fmt.Errorf("pool %d: %v", i, err)
		}
	}

	if numMainPool != 1 {
		return fmt.Errorf("one and only one mainPool is required")
	}
	if s.ConnectTimeout != "" {
		if _, err := time.ParseDuration(s.ConnectTimeout); err != nil {
			return fmt.Errorf("grpc client wait connection ready timeout %s invalid", s.ConnectTimeout)
		}
	}
	if s.BorrowTimeout != "" {
		if _, err := time.ParseDuration(s.BorrowTimeout); err != nil {
			return fmt.Errorf("grpc proxy filter wait get a conenction timeout %s invalid", s.BorrowTimeout)
		}
	}
	if s.Timeout != "" {
		if _, err := time.ParseDuration(s.Timeout); err != nil {
			return fmt.Errorf("grpc proxy filter process request timeout %s invalid", s.BorrowTimeout)
		}
	}

	if s.InitConnsPerHost < 0 || s.MaxIdleConnsPerHost <= 0 || s.MaxIdleConnsPerHost < s.InitConnsPerHost {
		return fmt.Errorf("grpc max connection num %d or init connection num %d per host invalid", s.MaxIdleConnsPerHost, s.InitConnsPerHost)
	}

	return nil
}

// Name returns the name of the Proxy filter instance.
func (p *Proxy) Name() string {
	return p.spec.Name()
}

// Kind returns the kind of Proxy.
func (p *Proxy) Kind() *filters.Kind {
	return kind
}

// Spec returns the spec used by the Proxy
func (p *Proxy) Spec() filters.Spec {
	return p.spec
}

// Init initializes Proxy.
func (p *Proxy) Init() {
	p.reload()
}

// Inherit inherits previous generation of Proxy.
func (p *Proxy) Inherit(previousGeneration filters.Filter) {
	p.reload()
}

func (p *Proxy) reload() {
	for _, spec := range p.spec.Pools {
		name := ""
		if spec.Filter == nil {
			name = fmt.Sprintf("proxy#%s#main", p.Name())
		} else {
			id := len(p.candidatePools)
			name = fmt.Sprintf("proxy#%s#candidate#%d", p.Name(), id)
		}

		pool := NewServerPool(p, spec, name)

		if spec.Filter == nil {
			p.mainPool = pool
		} else {
			p.candidatePools = append(p.candidatePools, pool)
		}
	}

	p.borrowTimeout, _ = time.ParseDuration(p.spec.BorrowTimeout)
	p.timeout, _ = time.ParseDuration(p.spec.Timeout)
	p.connectTimeout, _ = time.ParseDuration(p.spec.ConnectTimeout)

	if p.pool == nil {
		p.poolSpec = &objectpool.Spec{
			InitSize:     p.spec.InitConnsPerHost,
			MaxSize:      p.spec.MaxIdleConnsPerHost,
			CheckWhenPut: true,
			CheckWhenGet: true,
			New: func(ctx stdcontext.Context) (objectpool.PoolObject, error) {
				target := objectpool.GetSeparatedKey(ctx)
				dialCtx, cancel := stdcontext.WithCancel(stdcontext.Background())
				if p.connectTimeout != 0 {
					dialCtx, cancel = stdcontext.WithTimeout(dialCtx, p.connectTimeout)
				}
				defer cancel()
				conn, err := grpc.DialContext(dialCtx, target, defaultDialOpts...)
				if err != nil {
					logger.Infof("create new grpc client connection for %s fail %v", target, err)
					return nil, err
				}
				return &clientConnWrapper{conn}, nil
			},
		}
		p.pool = objectpool.NewMultiWithSpec(p.poolSpec)
	} else {
		p.poolSpec.MaxSize = p.spec.MaxIdleConnsPerHost
		p.poolSpec.InitSize = p.spec.InitConnsPerHost
	}
}

// Status returns Proxy status.
func (p *Proxy) Status() interface{} {
	return nil
}

// Close closes Proxy.
func (p *Proxy) Close() {
	p.mainPool.Close()

	for _, v := range p.candidatePools {
		v.Close()
	}
}

// Handle handles GRPCContext.
func (p *Proxy) Handle(ctx *context.Context) (result string) {
	req := ctx.GetInputRequest().(*grpcprot.Request)
	sp := p.mainPool
	for _, v := range p.candidatePools {
		if v.filter.Match(req) {
			sp = v
			break
		}
	}

	return sp.handle(ctx)
}

// InjectResiliencePolicy injects resilience policies to the proxy.
func (p *Proxy) InjectResiliencePolicy(policies map[string]resilience.Policy) {
	p.mainPool.InjectResiliencePolicy(policies)

	for _, sp := range p.candidatePools {
		sp.InjectResiliencePolicy(policies)
	}
}
