// Copyright (c) 2015 Jan Broer. All rights reserved.
// Use of this source code is governed by The MIT License (MIT) that can be
// found in the LICENSE file.

package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/bingoohuang/gg/pkg/v"
	"github.com/bingoohuang/godaemon"
	"github.com/bingoohuang/rotatefile/homedir"
	_ "github.com/bingoohuang/rotatefile/stdlog/autoload"
	"github.com/soulteary/go-dnsmasq/pkg"
	"github.com/soulteary/go-dnsmasq/pkg/resolvconf"
	"github.com/soulteary/go-dnsmasq/pkg/server"
	"github.com/soulteary/go-dnsmasq/pkg/types"
	"github.com/urfave/cli"
)

var Version = strings.ReplaceAll(v.Version(), "\n", " ")

func main() {
	app := cli.NewApp()
	app.Name = "go-dnsmasq"
	app.Usage = "Lightweight caching DNS server and forwarder\n   Website: http://github.com/soulteary/go-dnsmasq"
	app.UsageText = "go-dnsmasq [global options]"
	app.Version = Version
	app.Author, app.Email = "", ""
	app.Flags = []cli.Flag{
		cli.StringFlag{Name: "log-level", Value: "info", Usage: "log level", EnvVar: types.LogLevel},
		cli.StringFlag{
			Name: "listen, l", Value: "127.0.0.1:53", EnvVar: types.Listen,
			Usage: "Listen on this `address` <host[:port]>",
		},
		cli.BoolFlag{
			Name: "default-resolver, d", EnvVar: types.DefaultResolver,
			Usage: "Update /etc/resolv.conf with the address of go-dnsmasq as nameserver",
		},
		cli.StringSliceFlag{
			Name: "nameservers, n", EnvVar: types.NameServers,
			Usage: "Comma delimited list of `nameservers` <host[:port][,host[:port]]> (supersedes resolv.conf)",
		},
		cli.StringSliceFlag{
			Name: "stubzones, z", EnvVar: types.StubZone,
			Usage: "Use different nameservers for given domains <domain[,domain]/host[:port][,host[:port]]>",
		},
		cli.StringFlag{
			Name: "hostsfile, f", EnvVar: types.HostsFile,
			Usage: "Path to a hosts `file` (e.g. /etc/hosts)",
		},
		cli.StringFlag{
			Name: "resolve", EnvVar: types.HostsFile,
			Usage: "Resolve domain name by specified dns server (e.g. www.google.com@8.8.8.8:53)",
		},
		cli.StringFlag{
			Name: "hostsfiles, fs", EnvVar: types.HostsDirectory,
			Usage: "Path to the `directory` of hosts file (e.g. /etc/host)",
		},
		cli.DurationFlag{
			Name: "hostsfile-poll, p", Value: 0, EnvVar: types.HostsFilePollDuration,
			Usage: "How frequently to poll hosts file (`1s`, '0' to disable)",
		},
		cli.StringSliceFlag{
			Name: "search-domains, s", EnvVar: types.SearchDomains,
			Usage: "List of search domains <domain[,domain]> (supersedes resolv.conf)",
		},
		cli.BoolFlag{
			Name: "enable-search, search", EnvVar: types.EnableSearch,
			Usage: "Qualify names with search domains to resolve queries",
		},
		cli.IntFlag{
			Name: "rcache, r", Value: 0, EnvVar: types.ResponseCacheCap,
			Usage: "Response cache `capacity` ('0' disables caching)",
		},
		cli.DurationFlag{
			Name: "rcache-ttl", Value: time.Minute, EnvVar: types.ResponseCacheTTL,
			Usage: "TTL for response cache entries",
		},
		cli.BoolFlag{Name: "no-rec", Usage: "Disable recursion", EnvVar: types.DisableRecursion},
		cli.IntFlag{
			Name: "fwd-ndots", EnvVar: types.FwdNdots,
			Usage: "Number of `dots` a name must have before the query is forwarded",
		},
		cli.IntFlag{
			Name: "ndots", Value: 1, EnvVar: types.Ndots,
			Usage: "Number of `dots` a name must have before doing an initial absolute query (supersedes resolv.conf)",
		},
		cli.BoolFlag{
			Name: "round-robin", EnvVar: types.RoundRobin,
			Usage: "Enable round robin of A/AAAA records",
		},
		cli.BoolFlag{
			Name: "systemd", EnvVar: types.Systemd,
			Usage: "Bind to socket activated by Systemd (supersedes '--listen')",
		},
		cli.BoolFlag{Name: "fg", Usage: "foreground", EnvVar: types.Foreground},
	}

	app.Action = func(c *cli.Context) error {
		if resolve := c.String("resolve"); resolve != "" {
			ips, err := Resolver(resolve)
			if err != nil {
				return err
			}

			fmt.Printf("%v", ips)
			return nil
		}

		log.Printf("Starting go-dnsmasq server %s", Version)

		nameservers, err := server.CreateNameservers(c.StringSlice("nameservers"))
		if err != nil {
			return err
		}

		searchDomains, err := server.CreateSearchDomains(c.StringSlice("search-domains"))
		if err != nil {
			return err
		}

		stubmap, err := server.CreateStubMap(c.StringSlice("stubzones"))
		if err != nil {
			return err
		}

		listen, err := server.CreateListenAddress(c.String("listen"))
		if err != nil {
			return err
		}

		config := &server.Config{
			DnsAddr:             listen,
			DefaultResolver:     c.Bool("default-resolver"),
			Nameservers:         nameservers,
			Systemd:             c.Bool("systemd"),
			Daemon:              !c.Bool("fg"),
			SearchDomains:       searchDomains,
			EnableSearch:        c.Bool("enable-search"),
			Hostsfile:           c.String("hostsfile"),
			DirectoryHostsfiles: c.String("hostsfiles"),
			PollInterval:        c.Duration("hostsfile-poll"),
			RoundRobin:          c.Bool("round-robin"),
			NoRec:               c.Bool("no-rec"),
			FwdNdots:            c.Int("fwd-ndots"),
			Ndots:               c.Int("ndots"),
			ReadTimeout:         2 * time.Second,
			RCache:              c.Int("rcache"),
			RCacheTtl:           c.Duration("rcache-ttl"),
			Verbose:             c.Bool("verbose"),
			Stub:                stubmap,
		}

		if config.Hostsfile == "" {
			if hosts, _ := homedir.Expand("~/.hosts"); hosts != "" {
				if stat, err := os.Stat(hosts); err == nil && !stat.IsDir() {
					config.Hostsfile = hosts
				}
			}
		}

		resolvconf.Clean()
		if err := server.ResolvConf(config, c.IsSet("ndots")); err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("parsing resolve.conf: %w", err)
			}
		}

		s, err := pkg.BuildServer(config, nil, Version)
		if err != nil {
			return err
		}

		if config.Daemon {
			godaemon.Daemonize()
		}

		return pkg.Run(s)
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

// Resolver 指定 dnsServer (例 "8.8.8.8:53")  解析 domainName (例 "www.google.com")
func Resolver(domainName string) ([]string, error) {
	atPos := strings.IndexByte(domainName, '@')
	if atPos < 0 {
		return net.LookupHost(domainName)
	}

	dnsServer := domainName[atPos+1:]
	domainName = domainName[:atPos]

	if _, port, _ := net.SplitHostPort(dnsServer); port == "" {
		dnsServer += ":53"
	}

	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{
				Timeout: time.Millisecond * time.Duration(10000),
			}
			return d.DialContext(ctx, network, dnsServer)
		},
	}
	ip, err := r.LookupHost(context.Background(), domainName)
	return ip, err
}
