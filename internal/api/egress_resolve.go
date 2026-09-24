package api

import (
	"prism-2api/internal/egress"
	"prism-2api/internal/pool"
	"prism-2api/internal/proxypool"
)

func (s *Server) resolveAccountEgress(acc *pool.Account) egress.Settings {
	accountID := ""
	if acc != nil {
		accountID = acc.StableIdentity()
	}
	if acc != nil {
		if pid := acc.ProxyID(); pid != "" && s.proxies != nil {
			if p, ok := s.proxies.Get(pid); ok && p.Enabled {
				return proxypool.SettingsFor(p, accountID)
			}
		}
		if u := acc.ProxyURL(); u != "" {
			return egress.Settings{Kind: egress.KindHTTP, HTTPProxyURL: u, Account: accountID}
		}
	}
	if s.proxies != nil {
		if p, ok := s.proxies.Default(); ok {
			return proxypool.SettingsFor(p, accountID)
		}
	}
	if s.runtime != nil && s.runtime.ResinOn() {
		url, plat := s.runtime.ResinEndpoint()
		return egress.Settings{Kind: egress.KindResin, ResinURL: url, ResinPlatform: plat, Account: accountID}
	}
	if s.runtime != nil {
		if gp := s.runtime.GlobalProxy(); gp != "" {
			return egress.Settings{Kind: egress.KindHTTP, HTTPProxyURL: gp, Account: accountID}
		}
	}
	if s.cfg != nil && s.cfg.ProxyURL != "" {
		return egress.Settings{Kind: egress.KindHTTP, HTTPProxyURL: s.cfg.ProxyURL, Account: accountID}
	}
	return egress.Settings{Kind: egress.KindDirect, Account: accountID}
}
