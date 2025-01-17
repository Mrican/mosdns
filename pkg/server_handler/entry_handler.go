/*
 * Copyright (C) 2020-2022, IrineSistiana
 *
 * This file is part of mosdns.
 *
 * mosdns is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * mosdns is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package server_handler

import (
	"context"
	"time"

	"github.com/IrineSistiana/mosdns/v5/mlog"
	"github.com/IrineSistiana/mosdns/v5/pkg/dnsutils"
	"github.com/IrineSistiana/mosdns/v5/pkg/edns0ede"
	"github.com/IrineSistiana/mosdns/v5/pkg/query_context"
	"github.com/IrineSistiana/mosdns/v5/pkg/server"
	"github.com/IrineSistiana/mosdns/v5/pkg/utils"
	"github.com/IrineSistiana/mosdns/v5/plugin/executable/sequence"
	"github.com/miekg/dns"
	"go.uber.org/zap"
)

const (
	defaultQueryTimeout = time.Second * 5
	edns0Size           = 1220
)

var (
	nopLogger = mlog.Nop()

	// options that can forward to upstream
	queryForwardEDNS0Option = map[uint16]struct{}{
		dns.EDNS0SUBNET: {},
	}

	// options that useless for downstream
	respRemoveEDNS0Option = map[uint16]struct{}{
		dns.EDNS0PADDING: {},
	}
)

type EntryHandlerOpts struct {
	// Logger is used for logging. Default is a noop logger.
	Logger *zap.Logger

	// Required.
	Entry sequence.Executable

	// QueryTimeout limits the timeout value of each query.
	// Default is defaultQueryTimeout.
	QueryTimeout time.Duration
}

func (opts *EntryHandlerOpts) init() {
	if opts.Logger == nil {
		opts.Logger = nopLogger
	}
	utils.SetDefaultNum(&opts.QueryTimeout, defaultQueryTimeout)
}

type EntryHandler struct {
	opts EntryHandlerOpts
}

var _ server.Handler = (*EntryHandler)(nil)

func NewEntryHandler(opts EntryHandlerOpts) *EntryHandler {
	opts.init()
	return &EntryHandler{opts: opts}
}

// ServeDNS implements server.Handler.
// If entry returns an error, a SERVFAIL response will be returned.
// If entry returns without a response, a REFUSED response will be returned.
func (h *EntryHandler) Handle(ctx context.Context, q *dns.Msg, serverMeta server.QueryMeta, packMsgPayload func(m *dns.Msg) (*[]byte, error)) *[]byte {
	ddl := time.Now().Add(h.opts.QueryTimeout)
	ctx, cancel := context.WithDeadline(ctx, ddl)
	defer cancel()

	// Get udp size before exec plugins. It may be changed by plugins.
	queryUdpSize := getUDPSize(q)

	// Enable edns0. We can handle this.
	// This also helps to avoid udp->tcp fallback.
	queryOpt, queryEDNS0Upgraded := enableEdns0(q)
	queryHasEDNS0 := !queryEDNS0Upgraded

	// Save all query opts for plugins.
	allQueryOpts := queryOpt.Option
	queryOpt.Option = filterQueryOptions2Forward(queryOpt)

	// exec entry
	qCtx := query_context.NewContext(q)
	qCtx.ServerMeta = serverMeta
	qCtx.QueryOpt = allQueryOpts
	err := h.opts.Entry.Exec(ctx, qCtx)
	resp := qCtx.R()
	if resp == nil {
		resp = new(dns.Msg)
		resp.SetReply(qCtx.Q())
		resp.Rcode = dns.RcodeRefused
	}

	// May be nil
	var respOpt *dns.OPT
	if queryHasEDNS0 {
		// RFC 3225 3
		// The DO bit of the query MUST be copied in the response.
		// ...
		// The absence of DNSSEC data in response to a query with the DO bit set
		// MUST NOT be taken to mean no security information is available for
		// that zone as the response may be forged or a non-forged response of
		// an altered (DO bit cleared) query.
		var newOpt bool
		respOpt, newOpt = enableEdns0(resp)
		if queryOpt.Do() {
			setDo(respOpt, true)
		}
		if !newOpt {
			filterUselessRespOpt(respOpt)
		}
	} else {
		// Remove edns0 from resp if client didn't send it, as RFC 2671 required.
		dnsutils.RemoveEDNS0(resp)
	}

	if err != nil {
		resp.Rcode = dns.RcodeServerFailure
		switch v := err.(type) {
		case *edns0ede.EdeError:
			h.opts.Logger.Warn("entry err", qCtx.InfoField(), zap.Object("ede", v))
			if respOpt != nil {
				respOpt.Option = append(respOpt.Option, (*dns.EDNS0_EDE)(v))
			}
		case *edns0ede.EdeErrors:
			h.opts.Logger.Warn("entry err", qCtx.InfoField(), zap.Array("edes", v))
			if respOpt != nil {
				for _, ede := range ([]*dns.EDNS0_EDE)(*v) {
					respOpt.Option = append(respOpt.Option, ede)
				}
			}
		default:
			h.opts.Logger.Warn("entry err", qCtx.InfoField(), zap.Error(err))
		}
	}

	// We assume that our server is a forwarder.
	resp.RecursionAvailable = true

	if serverMeta.FromUDP {
		resp.Truncate(queryUdpSize)
	}

	payload, err := packMsgPayload(resp)
	if err != nil {
		h.opts.Logger.Error("internal err: failed to pack resp msg", qCtx.InfoField(), zap.Error(err))
		return nil
	}
	return payload
}

func getUDPSize(m *dns.Msg) int {
	var s uint16
	if opt := m.IsEdns0(); opt != nil {
		s = opt.UDPSize()
	}
	if s < dns.MinMsgSize {
		s = dns.MinMsgSize
	}
	return int(s)
}

// returns a copy
func filterQueryOptions2Forward(opt *dns.OPT) []dns.EDNS0 {
	var remainOpt []dns.EDNS0
	for _, op := range opt.Option {
		if _, ok := queryForwardEDNS0Option[op.Option()]; ok {
			remainOpt = append(remainOpt, op)
		}
	}
	return remainOpt
}

// modifies opt directly
func filterUselessRespOpt(opt *dns.OPT) {
	remainOpt := opt.Option[0:0]
	for _, op := range opt.Option {
		if _, remove := respRemoveEDNS0Option[op.Option()]; !remove {
			remainOpt = append(remainOpt, op)
		}
	}
	opt.Option = remainOpt
}

func enableEdns0(m *dns.Msg) (*dns.OPT, bool) {
	opt := m.IsEdns0()
	edns0Upgraded := false
	if opt == nil {
		opt = new(dns.OPT)
		opt.Hdr.Name = "."
		opt.Hdr.Rrtype = dns.TypeOPT
		m.Extra = append(m.Extra, opt)
		edns0Upgraded = true
	}
	opt.SetUDPSize(edns0Size)
	return opt, edns0Upgraded
}

func setDo(opt *dns.OPT, do bool) {
	const doBit = 1 << 15 // DNSSEC OK
	if do {
		opt.Hdr.Ttl |= doBit
	}
}
