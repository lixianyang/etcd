// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package etcdserver

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/coreos/etcd/pkg/netutil"
	"github.com/coreos/etcd/pkg/transport"
	"github.com/coreos/etcd/pkg/types"

	"go.uber.org/zap"
)

// ServerConfig holds the configuration of etcd as taken from the command line or discovery.
type ServerConfig struct {
	Name           string
	DiscoveryURL   string
	DiscoveryProxy string
	ClientURLs     types.URLs
	PeerURLs       types.URLs
	DataDir        string
	// DedicatedWALDir config will make the etcd to write the WAL to the WALDir
	// rather than the dataDir/member/wal.
	DedicatedWALDir     string
	SnapCount           uint64
	MaxSnapFiles        uint
	MaxWALFiles         uint
	InitialPeerURLsMap  types.URLsMap
	InitialClusterToken string
	NewCluster          bool
	PeerTLSInfo         transport.TLSInfo

	CORS map[string]struct{}

	// HostWhitelist lists acceptable hostnames from client requests.
	// If server is insecure (no TLS), server only accepts requests
	// whose Host header value exists in this white list.
	HostWhitelist map[string]struct{}

	TickMs           uint
	ElectionTicks    int
	BootstrapTimeout time.Duration

	AutoCompactionRetention time.Duration
	AutoCompactionMode      string
	QuotaBackendBytes       int64
	MaxTxnOps               uint

	// MaxRequestBytes is the maximum request size to send over raft.
	MaxRequestBytes uint

	StrictReconfigCheck bool

	// ClientCertAuthEnabled is true when cert has been signed by the client CA.
	ClientCertAuthEnabled bool

	AuthToken string

	// InitialCorruptCheck is true to check data corruption on boot
	// before serving any peer/client traffic.
	InitialCorruptCheck bool
	CorruptCheckTime    time.Duration

	// PreVote is true to enable Raft Pre-Vote.
	PreVote bool

	// Logger logs server-side operations.
	// If not nil, it disables "capnslog" and uses the given logger.
	Logger *zap.Logger
	// LoggerConfig is server logger configuration for Raft logger.
	LoggerConfig zap.Config

	Debug bool

	ForceNewCluster bool
}

// VerifyBootstrap sanity-checks the initial config for bootstrap case
// and returns an error for things that should never happen.
func (c *ServerConfig) VerifyBootstrap() error {
	if err := c.hasLocalMember(); err != nil {
		return err
	}
	if err := c.advertiseMatchesCluster(); err != nil {
		return err
	}
	if checkDuplicateURL(c.InitialPeerURLsMap) {
		return fmt.Errorf("initial cluster %s has duplicate url", c.InitialPeerURLsMap)
	}
	if c.InitialPeerURLsMap.String() == "" && c.DiscoveryURL == "" {
		return fmt.Errorf("initial cluster unset and no discovery URL found")
	}
	return nil
}

// VerifyJoinExisting sanity-checks the initial config for join existing cluster
// case and returns an error for things that should never happen.
func (c *ServerConfig) VerifyJoinExisting() error {
	// The member has announced its peer urls to the cluster before starting; no need to
	// set the configuration again.
	if err := c.hasLocalMember(); err != nil {
		return err
	}
	if checkDuplicateURL(c.InitialPeerURLsMap) {
		return fmt.Errorf("initial cluster %s has duplicate url", c.InitialPeerURLsMap)
	}
	if c.DiscoveryURL != "" {
		return fmt.Errorf("discovery URL should not be set when joining existing initial cluster")
	}
	return nil
}

// hasLocalMember checks that the cluster at least contains the local server.
func (c *ServerConfig) hasLocalMember() error {
	if urls := c.InitialPeerURLsMap[c.Name]; urls == nil {
		return fmt.Errorf("couldn't find local name %q in the initial cluster configuration", c.Name)
	}
	return nil
}

// advertiseMatchesCluster confirms peer URLs match those in the cluster peer list.
func (c *ServerConfig) advertiseMatchesCluster() error {
	urls, apurls := c.InitialPeerURLsMap[c.Name], c.PeerURLs.StringSlice()
	urls.Sort()
	sort.Strings(apurls)
	ctx, cancel := context.WithTimeout(context.TODO(), 30*time.Second)
	defer cancel()
	ok, err := netutil.URLStringsEqual(ctx, apurls, urls.StringSlice())
	if ok {
		return nil
	}

	initMap, apMap := make(map[string]struct{}), make(map[string]struct{})
	for _, url := range c.PeerURLs {
		apMap[url.String()] = struct{}{}
	}
	for _, url := range c.InitialPeerURLsMap[c.Name] {
		initMap[url.String()] = struct{}{}
	}

	missing := []string{}
	for url := range initMap {
		if _, ok := apMap[url]; !ok {
			missing = append(missing, url)
		}
	}
	if len(missing) > 0 {
		for i := range missing {
			missing[i] = c.Name + "=" + missing[i]
		}
		mstr := strings.Join(missing, ",")
		apStr := strings.Join(apurls, ",")
		return fmt.Errorf("--initial-cluster has %s but missing from --initial-advertise-peer-urls=%s (%v)", mstr, apStr, err)
	}

	for url := range apMap {
		if _, ok := initMap[url]; !ok {
			missing = append(missing, url)
		}
	}
	if len(missing) > 0 {
		mstr := strings.Join(missing, ",")
		umap := types.URLsMap(map[string]types.URLs{c.Name: c.PeerURLs})
		return fmt.Errorf("--initial-advertise-peer-urls has %s but missing from --initial-cluster=%s", mstr, umap.String())
	}

	// resolved URLs from "--initial-advertise-peer-urls" and "--initial-cluster" did not match or failed
	apStr := strings.Join(apurls, ",")
	umap := types.URLsMap(map[string]types.URLs{c.Name: c.PeerURLs})
	return fmt.Errorf("failed to resolve %s to match --initial-cluster=%s (%v)", apStr, umap.String(), err)
}

func (c *ServerConfig) MemberDir() string { return filepath.Join(c.DataDir, "member") }

func (c *ServerConfig) WALDir() string {
	if c.DedicatedWALDir != "" {
		return c.DedicatedWALDir
	}
	return filepath.Join(c.MemberDir(), "wal")
}

func (c *ServerConfig) SnapDir() string { return filepath.Join(c.MemberDir(), "snap") }

func (c *ServerConfig) ShouldDiscover() bool { return c.DiscoveryURL != "" }

// ReqTimeout returns timeout for request to finish.
func (c *ServerConfig) ReqTimeout() time.Duration {
	// 5s for queue waiting, computation and disk IO delay
	// + 2 * election timeout for possible leader election
	return 5*time.Second + 2*time.Duration(c.ElectionTicks*int(c.TickMs))*time.Millisecond
}

func (c *ServerConfig) electionTimeout() time.Duration {
	return time.Duration(c.ElectionTicks*int(c.TickMs)) * time.Millisecond
}

func (c *ServerConfig) peerDialTimeout() time.Duration {
	// 1s for queue wait and election timeout
	return time.Second + time.Duration(c.ElectionTicks*int(c.TickMs))*time.Millisecond
}

func (c *ServerConfig) PrintWithInitial() { c.print(true) }

func (c *ServerConfig) Print() { c.print(false) }

func (c *ServerConfig) print(initial bool) {
	// TODO: remove this after dropping "capnslog"
	if c.Logger == nil {
		plog.Infof("name = %s", c.Name)
		if c.ForceNewCluster {
			plog.Infof("force new cluster")
		}
		plog.Infof("data dir = %s", c.DataDir)
		plog.Infof("member dir = %s", c.MemberDir())
		if c.DedicatedWALDir != "" {
			plog.Infof("dedicated WAL dir = %s", c.DedicatedWALDir)
		}
		plog.Infof("heartbeat = %dms", c.TickMs)
		plog.Infof("election = %dms", c.ElectionTicks*int(c.TickMs))
		plog.Infof("snapshot count = %d", c.SnapCount)
		if len(c.DiscoveryURL) != 0 {
			plog.Infof("discovery URL= %s", c.DiscoveryURL)
			if len(c.DiscoveryProxy) != 0 {
				plog.Infof("discovery proxy = %s", c.DiscoveryProxy)
			}
		}
		plog.Infof("advertise client URLs = %s", c.ClientURLs)
		if initial {
			plog.Infof("initial advertise peer URLs = %s", c.PeerURLs)
			plog.Infof("initial cluster = %s", c.InitialPeerURLsMap)
		}
	} else {
		caddrs := make([]string, len(c.ClientURLs))
		for i := range c.ClientURLs {
			caddrs[i] = c.ClientURLs[i].String()
		}
		paddrs := make([]string, len(c.PeerURLs))
		for i := range c.PeerURLs {
			paddrs[i] = c.PeerURLs[i].String()
		}
		state := "new"
		if !c.NewCluster {
			state = "existing"
		}
		c.Logger.Info(
			"server starting",
			zap.String("name", c.Name),
			zap.String("data-dir", c.DataDir),
			zap.String("member-dir", c.MemberDir()),
			zap.String("dedicated-wal-dir", c.DedicatedWALDir),
			zap.Bool("force-new-cluster", c.ForceNewCluster),
			zap.Uint("heartbeat-tick-ms", c.TickMs),
			zap.String("heartbeat-interval", fmt.Sprintf("%v", time.Duration(c.TickMs)*time.Millisecond)),
			zap.Int("election-tick-ms", c.ElectionTicks),
			zap.String("election-timeout", fmt.Sprintf("%v", time.Duration(c.ElectionTicks*int(c.TickMs))*time.Millisecond)),
			zap.Uint64("snapshot-count", c.SnapCount),
			zap.Strings("advertise-client-urls", caddrs),
			zap.Strings("initial-advertise-peer-urls", paddrs),
			zap.Bool("initial", initial),
			zap.String("initial-cluster", c.InitialPeerURLsMap.String()),
			zap.String("initial-cluster-state", state),
			zap.String("initial-cluster-token", c.InitialClusterToken),
			zap.Bool("pre-vote", c.PreVote),
			zap.Bool("initial-corrupt-check", c.InitialCorruptCheck),
			zap.Duration("corrupt-check-time", c.CorruptCheckTime),
			zap.String("discovery-url", c.DiscoveryURL),
			zap.String("discovery-proxy", c.DiscoveryProxy),
		)
	}
}

func checkDuplicateURL(urlsmap types.URLsMap) bool {
	um := make(map[string]bool)
	for _, urls := range urlsmap {
		for _, url := range urls {
			u := url.String()
			if um[u] {
				return true
			}
			um[u] = true
		}
	}
	return false
}

func (c *ServerConfig) bootstrapTimeout() time.Duration {
	if c.BootstrapTimeout != 0 {
		return c.BootstrapTimeout
	}
	return time.Second
}

func (c *ServerConfig) backendPath() string { return filepath.Join(c.SnapDir(), "db") }
