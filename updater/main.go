package main

import (
	"bytes"
	crand "crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"encoding/json"
	"net/http"

	"github.com/go-co-op/gocron"
	"github.com/joho/godotenv"
	mapmodels "github.com/notrustverify/alephium-fullnodes-map"
	"golang.org/x/crypto/blake2b"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type NumNodesDb struct {
	gorm.Model
	Count int
}

type BrokerInfo struct {
	CliqueID  []byte
	BrokerID  int
	BrokerNum int
	Address   string
	Port      int
}

var IPINFO_TOKEN string
var db *gorm.DB

const ONE_WEEK_HOUR = 168

const (
	defaultDiscoveryPort    = 9973
	defaultNetworkID        = 0
	defaultDiscoveryDepth   = 2
	defaultDiscoveryTimeout = 3 * time.Second
	defaultTCPTimeout       = 5 * time.Second
	defaultTCPWorkers       = 30
	apiProbeTimeout         = 3 * time.Second
)

var apiPorts = []int{12973, 443, 80}

const (
	signatureLength          = 64
	cliqueIDLength           = 33
	sessionIDLength          = 32
	discoveryVersion   int64 = 65536
	codePing                 = 0
	codePong                 = 1
	codeFindNode             = 2
	codeNeighbors            = 3
	codeHello                = 0
	defaultPingTimeout       = 1 * time.Second
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile | log.Lmicroseconds)

	err := godotenv.Load(".env")
	if err != nil {
		log.Printf("Warning: Failed to load .env file: %v", err)
	}

	dbPath := os.Getenv("DB_PATH")
	IPINFO_TOKEN = os.Getenv("IPINFO_TOKEN")
	cronUpdate := os.Getenv("CRON_INTERVAL")
	fullnodesList := strings.Split(os.Getenv("FULLNODE_LIST"), ",")
	networkID := readIntEnv("NETWORK_ID", defaultNetworkID)
	discoveryDepth := readIntEnv("DISCOVERY_DEPTH", defaultDiscoveryDepth)

	if len(fullnodesList) <= 0 {
		log.Fatalf("Configuration error: FULLNODE_LIST is empty")
	}

	conn, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		log.Fatalf("Database connection error: %v", err)
	}
	db = conn

	if err := db.AutoMigrate(&mapmodels.FullnodeDb{}, &NumNodesDb{}); err != nil {
		log.Fatalf("Database migration failed: %v", err)
	}
	log.Printf("Starting updater service with %d seeds, networkId=%d", len(fullnodesList), networkID)

	s := gocron.NewScheduler(time.UTC)
	s.Every(cronUpdate).Do(updateFullnodeList, fullnodesList, networkID, discoveryDepth)
	s.StartBlocking()
}

func updateFullnodeList(fullnodesList []string, networkID int, discoveryDepth int) {
	startTime := time.Now()
	log.Printf("Starting fullnode update at %v", startTime.Format(time.RFC3339))

	// Load all known nodes from DB for API scanning
	var allDBNodes []mapmodels.FullnodeDb
	if err := db.Find(&allDBNodes).Error; err != nil {
		log.Printf("Failed to load DB nodes for API scan: %v", err)
	}

	// Phase 1: Ping+FindNode on known nodes
	aliveNodes, livenessNeighbors := checkKnownNodes(networkID)

	// Build known endpoints set from alive nodes
	knownEndpoints := make(map[string]struct{}, len(aliveNodes))
	for _, n := range aliveNodes {
		knownEndpoints[fmt.Sprintf("%s:%d", n.Ip, n.Port)] = struct{}{}
	}

	// Pick up to 5 random alive nodes as extra seeds for discovery
	const maxExtraSeeds = 5
	extraSeeds := pickRandomAliveSeeds(aliveNodes, maxExtraSeeds)
	allSeeds := append(fullnodesList, extraSeeds...)

	// Phase 2 & 3 run in parallel: UDP discovery + REST API discovery on all DB nodes
	var newNodes []mapmodels.FullnodeDb
	var apiNodes []mapmodels.FullnodeDb
	phase23 := sync.WaitGroup{}

	phase23.Add(1)
	go func() {
		defer phase23.Done()
		newNodes = discoverNewNodes(allSeeds, networkID, discoveryDepth, knownEndpoints, livenessNeighbors)
	}()

	phase23.Add(1)
	go func() {
		defer phase23.Done()
		apiNodes = discoverViaAPI(allDBNodes, knownEndpoints)
	}()

	phase23.Wait()

	// Merge: deduplicate by ip:port; API nodes have richer data (clientVersion, isSynced)
	merged := make(map[string]mapmodels.FullnodeDb)
	for _, n := range aliveNodes {
		key := fmt.Sprintf("%s:%d", n.Ip, n.Port)
		merged[key] = n
	}
	for _, n := range newNodes {
		key := fmt.Sprintf("%s:%d", n.Ip, n.Port)
		merged[key] = n
	}
	for _, n := range apiNodes {
		key := fmt.Sprintf("%s:%d", n.Ip, n.Port)
		merged[key] = n
	}

	fullnodes := make([]mapmodels.FullnodeDb, 0, len(merged))
	for _, n := range merged {
		fullnodes = append(fullnodes, n)
	}

	numNodes := NumNodesDb{Count: len(fullnodes)}
	if result := db.Create(&numNodes); result.Error != nil {
		log.Printf("Failed to update node count in database: %v", result.Error)
	}

	log.Printf("Merge: %d alive + %d new(UDP) + %d new(API) = %d total nodes", len(aliveNodes), len(newNodes), len(apiNodes), len(fullnodes))

	if len(fullnodes) == 0 {
		log.Printf("No peers found; skipping fullnode upsert")
		return
	}

	result := db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "ip"}, {Name: "port"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"client_version", "is_synced", "group_num_per_broker", "port", "broker_id", "updated_at",
		}),
	}).Create(&fullnodes)

	if result.Error != nil {
		log.Printf("Failed to update fullnodes in database: %v", result.Error)
		return
	}
	log.Printf("Successfully upserted %d fullnode records", result.RowsAffected)

	var emptyIpFullnodes []mapmodels.FullnodeDb
	resultEmpty := db.Where("location = ? OR ip_updated_at < ? OR ip_updated_at is NULL", "", time.Now().Add(-(time.Hour * ONE_WEEK_HOUR))).Find(&emptyIpFullnodes)
	if resultEmpty.Error != nil {
		log.Printf("Failed to query nodes needing location updates: %v", resultEmpty.Error)
		return
	}

	if resultEmpty.RowsAffected > 0 {
		log.Printf("Found %d nodes needing location updates", resultEmpty.RowsAffected)
		ipInfo := getIpInfo(&emptyIpFullnodes)

		updateCount := 0
		for k, v := range ipInfo {
			updateResult := db.Model(&mapmodels.FullnodeDb{}).Where("ip = ?", k).Updates(mapmodels.FullnodeDb{
				Hostname:    v.Hostname,
				City:        v.City,
				Region:      v.Region,
				Country:     v.Country,
				Location:    v.Loc,
				Org:         v.Org,
				Postal:      v.Postal,
				Timezone:    v.Timezone,
				IpUpdatedAt: time.Now(),
			})

			if updateResult.Error != nil {
				log.Printf("Failed to update location for IP %s: %v", k, updateResult.Error)
			} else {
				updateCount++
			}
		}
		log.Printf("Successfully updated locations for %d/%d nodes", updateCount, len(ipInfo))
	}

	duration := time.Since(startTime)
	log.Printf("Update completed in %v", duration)
}

func readIntEnv(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("Invalid %s=%q; using default %d", key, raw, fallback)
		return fallback
	}
	return parsed
}

func resolveToIP(host string) string {
	if ip := net.ParseIP(host); ip != nil {
		return host
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return ""
	}
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			return v4.String()
		}
	}
	return ips[0].String()
}

func parseSeedAddress(seed string) (string, error) {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return "", fmt.Errorf("empty seed")
	}

	apiSplitKey := strings.Split(seed, "@")
	if len(apiSplitKey) > 1 {
		seed = apiSplitKey[len(apiSplitKey)-1]
	}

	seed = strings.TrimPrefix(seed, "http://")
	seed = strings.TrimPrefix(seed, "https://")
	seed = strings.TrimSuffix(seed, "/")

	host, port, err := net.SplitHostPort(seed)
	if err == nil {
		return net.JoinHostPort(host, port), nil
	}
	if strings.Contains(err.Error(), "missing port in address") {
		return net.JoinHostPort(seed, strconv.Itoa(defaultDiscoveryPort)), nil
	}
	return "", err
}

func pickRandomAliveSeeds(aliveNodes []mapmodels.FullnodeDb, max int) []string {
	if len(aliveNodes) == 0 {
		return nil
	}
	indices := rand.Perm(len(aliveNodes))
	count := max
	if count > len(aliveNodes) {
		count = len(aliveNodes)
	}
	seeds := make([]string, count)
	for i := 0; i < count; i++ {
		n := aliveNodes[indices[i]]
		seeds[i] = fmt.Sprintf("%s:%d", n.Ip, n.Port)
	}
	return seeds
}

func checkKnownNodes(networkID int) ([]mapmodels.FullnodeDb, map[string]BrokerInfo) {
	var knownNodes []mapmodels.FullnodeDb
	if err := db.Find(&knownNodes).Error; err != nil {
		log.Printf("Failed to load known nodes from DB: %v", err)
		return nil, nil
	}
	if len(knownNodes) == 0 {
		return nil, nil
	}
	log.Printf("Pinging + FindNode on %d known nodes", len(knownNodes))

	type probeResult struct {
		node      mapmodels.FullnodeDb
		alive     bool
		neighbors []BrokerInfo
	}

	wg := sync.WaitGroup{}
	out := make(chan probeResult, len(knownNodes))
	sem := make(chan struct{}, defaultTCPWorkers)

	for _, node := range knownNodes {
		wg.Add(1)
		go func(n mapmodels.FullnodeDb) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			endpoint := fmt.Sprintf("%s:%d", n.Ip, n.Port)
			alive := pingNode(endpoint, networkID, defaultPingTimeout)
			var neighbors []BrokerInfo
			if alive {
				neighbors, _ = queryNeighbors(endpoint, networkID, defaultDiscoveryTimeout)
			}
			out <- probeResult{node: n, alive: alive, neighbors: neighbors}
		}(node)
	}
	wg.Wait()
	close(out)

	var aliveNodes []mapmodels.FullnodeDb
	discoveredPeers := make(map[string]BrokerInfo)
	for res := range out {
		if !res.alive {
			continue
		}
		res.node.IsSynced = true
		res.node.UpdatedAt = time.Now()
		aliveNodes = append(aliveNodes, res.node)

		for _, peer := range res.neighbors {
			if peer.Address == "" || peer.Port <= 0 || peer.Port > 65535 {
				continue
			}
			key := fmt.Sprintf("%s:%d", peer.Address, peer.Port)
			if _, exists := discoveredPeers[key]; !exists {
				discoveredPeers[key] = peer
			}
		}
	}

	log.Printf("Liveness: %d/%d alive, found %d peers via FindNode", len(aliveNodes), len(knownNodes), len(discoveredPeers))

	if len(aliveNodes) > 0 {
		versionNodes := make(map[string]BrokerInfo, len(aliveNodes))
		for _, n := range aliveNodes {
			key := fmt.Sprintf("%s:%d", n.Ip, n.Port)
			versionNodes[key] = BrokerInfo{Address: n.Ip, Port: int(n.Port)}
		}
		versions := fetchClientVersions(versionNodes, networkID)
		for i := range aliveNodes {
			key := fmt.Sprintf("%s:%d", aliveNodes[i].Ip, aliveNodes[i].Port)
			if v, ok := versions[key]; ok && v != "" {
				aliveNodes[i].ClientVersion = v
			}
		}
	}

	return aliveNodes, discoveredPeers
}

func discoverNewNodes(seedNodes []string, networkID int, depth int, knownEndpoints map[string]struct{}, extraPeers map[string]BrokerInfo) []mapmodels.FullnodeDb {
	neighbors := make(map[string]BrokerInfo)

	for k, v := range extraPeers {
		neighbors[k] = v
	}

	log.Printf("Running discovery scan with %d seeds, depth=%d (+ %d peers from liveness)", len(seedNodes), depth, len(extraPeers))
	discovered := discoverNeighbors(seedNodes, networkID, depth)
	for k, v := range discovered {
		neighbors[k] = v
	}
	log.Printf("Discovery found %d total peers, filtering against %d known", len(neighbors), len(knownEndpoints))

	newNodes := make(map[string]BrokerInfo)
	for key, node := range neighbors {
		if _, known := knownEndpoints[key]; known {
			continue
		}
		if node.Address == "" || node.Port <= 0 || node.Port > 65535 {
			continue
		}
		newNodes[key] = node
	}

	if len(newNodes) == 0 {
		log.Printf("No new nodes discovered")
		return nil
	}
	log.Printf("Found %d new nodes, fetching versions via TCP Hello", len(newNodes))

	versions := fetchClientVersions(newNodes, networkID)

	result := make([]mapmodels.FullnodeDb, 0, len(newNodes))
	for endpoint, node := range newNodes {
		result = append(result, mapmodels.FullnodeDb{
			CliqueId:          hex.EncodeToString(node.CliqueID),
			BrokerId:          uint(node.BrokerID),
			GroupNumPerBroker: uint(node.BrokerNum),
			Ip:                node.Address,
			Port:              uint(node.Port),
			IsSynced:          true,
			ClientVersion:     versions[endpoint],
		})
	}
	return result
}

func discoverNeighbors(seedNodes []string, networkID int, maxDepth int) map[string]BrokerInfo {
	if maxDepth < 1 {
		maxDepth = 1
	}

	neighbors := make(map[string]BrokerInfo)
	frontier := make([]string, 0, len(seedNodes))
	visited := make(map[string]struct{})

	for _, seed := range seedNodes {
		normalized, err := parseSeedAddress(seed)
		if err != nil {
			log.Printf("Skipping invalid seed %q: %v", seed, err)
			continue
		}
		frontier = append(frontier, normalized)
	}

	type discoveryResult struct {
		endpoint string
		peers    []BrokerInfo
		err      error
	}

	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		toQuery := make([]string, 0, len(frontier))
		for _, endpoint := range frontier {
			if _, seen := visited[endpoint]; seen {
				continue
			}
			visited[endpoint] = struct{}{}
			toQuery = append(toQuery, endpoint)
		}
		if len(toQuery) == 0 {
			break
		}

		log.Printf("Discovery round %d querying %d endpoints in parallel", depth+1, len(toQuery))

		results := make(chan discoveryResult, len(toQuery))
		wg := sync.WaitGroup{}
		for _, ep := range toQuery {
			wg.Add(1)
			go func(endpoint string) {
				defer wg.Done()
				peers, err := queryNeighbors(endpoint, networkID, defaultDiscoveryTimeout)
				results <- discoveryResult{endpoint: endpoint, peers: peers, err: err}
			}(ep)
		}
		wg.Wait()
		close(results)

		nextFrontier := make([]string, 0)
		for res := range results {
			if res.err != nil {
				log.Printf("Discovery query failed endpoint=%s err=%v", res.endpoint, res.err)
				continue
			}

			if _, exists := neighbors[res.endpoint]; !exists {
				host, portStr, splitErr := net.SplitHostPort(res.endpoint)
				if splitErr == nil {
					p, _ := strconv.Atoi(portStr)
					ip := resolveToIP(host)
					if ip != "" {
						key := fmt.Sprintf("%s:%d", ip, p)
						if _, exists := neighbors[key]; !exists {
							neighbors[key] = BrokerInfo{Address: ip, Port: p}
						}
					}
				}
			}

			for _, peer := range res.peers {
				if peer.Address == "" || peer.Port <= 0 || peer.Port > 65535 {
					continue
				}
				key := fmt.Sprintf("%s:%d", peer.Address, peer.Port)
				if _, exists := neighbors[key]; !exists {
					neighbors[key] = peer
					nextFrontier = append(nextFrontier, key)
				}
			}
		}
		frontier = nextFrontier
	}

	return neighbors
}

const findNodeQueries = 4

func queryNeighbors(endpoint string, networkID int, timeout time.Duration) ([]BrokerInfo, error) {
	conn, err := net.Dial("udp", endpoint)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	for i := 0; i < findNodeQueries; i++ {
		target := make([]byte, cliqueIDLength)
		_, _ = crand.Read(target)
		target[0] = 0x02
		msg, err := buildFindNodeMessage(networkID, target)
		if err != nil {
			return nil, err
		}
		if _, err := conn.Write(msg); err != nil {
			return nil, err
		}
	}

	seen := make(map[string]struct{})
	neighbors := make([]BrokerInfo, 0)
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	for {
		buf := make([]byte, 65535)
		n, err := conn.Read(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				break
			}
			return neighbors, err
		}

		parsed := extractNeighborsFromMessage(buf[:n], networkID)
		for _, item := range parsed {
			key := fmt.Sprintf("%s:%d", item.Address, item.Port)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			neighbors = append(neighbors, item)
		}
	}

	return neighbors, nil
}

func fetchClientVersions(nodes map[string]BrokerInfo, networkID int) map[string]string {
	results := make(map[string]string, len(nodes))
	if len(nodes) == 0 {
		return results
	}

	type versionResult struct {
		key     string
		version string
	}

	wg := sync.WaitGroup{}
	out := make(chan versionResult, len(nodes))
	sem := make(chan struct{}, defaultTCPWorkers)

	for key, node := range nodes {
		wg.Add(1)
		go func(endpoint string, peer BrokerInfo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			version := fetchClientVersionTCP(peer.Address, peer.Port, networkID, defaultTCPTimeout)
			out <- versionResult{key: endpoint, version: version}
		}(key, node)
	}

	wg.Wait()
	close(out)

	for item := range out {
		results[item.key] = item.version
	}
	return results
}

type interCliquePeer struct {
	CliqueId          string `json:"cliqueId"`
	BrokerId          int    `json:"brokerId"`
	GroupNumPerBroker int    `json:"groupNumPerBroker"`
	Address           struct {
		Addr string `json:"addr"`
		Port int    `json:"port"`
	} `json:"address"`
	IsSynced      bool   `json:"isSynced"`
	ClientVersion string `json:"clientVersion"`
}

func fetchInterCliquePeers(ip string) ([]interCliquePeer, error) {
	client := &http.Client{Timeout: apiProbeTimeout}
	for _, port := range apiPorts {
		url := fmt.Sprintf("http://%s:%d/infos/inter-clique-peer-info", ip, port)
		resp, err := client.Get(url)
		if err != nil {
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			continue
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			continue
		}
		var peers []interCliquePeer
		if err := json.Unmarshal(body, &peers); err != nil {
			continue
		}
		return peers, nil
	}
	return nil, fmt.Errorf("no open API port found for %s", ip)
}

func discoverViaAPI(aliveNodes []mapmodels.FullnodeDb, knownEndpoints map[string]struct{}) []mapmodels.FullnodeDb {
	if len(aliveNodes) == 0 {
		return nil
	}
	log.Printf("Probing %d alive nodes for REST API inter-clique peers", len(aliveNodes))

	type apiResult struct {
		peers []interCliquePeer
	}

	wg := sync.WaitGroup{}
	out := make(chan apiResult, len(aliveNodes))
	sem := make(chan struct{}, defaultTCPWorkers)

	for _, node := range aliveNodes {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			peers, err := fetchInterCliquePeers(ip)
			if err != nil {
				return
			}
			out <- apiResult{peers: peers}
		}(node.Ip)
	}
	wg.Wait()
	close(out)

	newPeers := make(map[string]interCliquePeer)
	for res := range out {
		for _, p := range res.peers {
			if p.Address.Addr == "" || p.Address.Port <= 0 || p.Address.Port > 65535 {
				continue
			}
			resolved := resolveToIP(p.Address.Addr)
			key := fmt.Sprintf("%s:%d", resolved, p.Address.Port)
			if _, known := knownEndpoints[key]; known {
				continue
			}
			if _, exists := newPeers[key]; !exists {
				p.Address.Addr = resolved
				newPeers[key] = p
			}
		}
	}

	if len(newPeers) == 0 {
		log.Printf("API discovery: no new peers found")
		return nil
	}
	log.Printf("API discovery: found %d new peers", len(newPeers))

	result := make([]mapmodels.FullnodeDb, 0, len(newPeers))
	for _, p := range newPeers {
		result = append(result, mapmodels.FullnodeDb{
			CliqueId:          p.CliqueId,
			BrokerId:          uint(p.BrokerId),
			GroupNumPerBroker: uint(p.GroupNumPerBroker),
			Ip:                p.Address.Addr,
			Port:              uint(p.Address.Port),
			IsSynced:          p.IsSynced,
			ClientVersion:     p.ClientVersion,
		})
	}
	return result
}

type ipInfoResult struct {
	Hostname string `json:"hostname"`
	City     string `json:"city"`
	Region   string `json:"region"`
	Country  string `json:"country"`
	Loc      string `json:"loc"`
	Org      string `json:"org"`
	Postal   string `json:"postal"`
	Timezone string `json:"timezone"`
}

func getIpInfo(fullnodes *[]mapmodels.FullnodeDb) map[string]ipInfoResult {
	results := make(map[string]ipInfoResult)
	if len(*fullnodes) == 0 {
		return results
	}
	if strings.TrimSpace(IPINFO_TOKEN) == "" {
		log.Printf("IPINFO_TOKEN is empty; skipping location enrichment")
		return results
	}

	type lookupResult struct {
		ip   string
		info ipInfoResult
		err  error
	}

	wg := sync.WaitGroup{}
	out := make(chan lookupResult, len(*fullnodes))
	sem := make(chan struct{}, defaultTCPWorkers)
	httpClient := &http.Client{Timeout: 10 * time.Second}

	for _, fn := range *fullnodes {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			url := fmt.Sprintf("https://ipinfo.io/%s?token=%s", ip, IPINFO_TOKEN)
			resp, err := httpClient.Get(url)
			if err != nil {
				out <- lookupResult{ip: ip, err: err}
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				out <- lookupResult{ip: ip, err: fmt.Errorf("status %d", resp.StatusCode)}
				return
			}
			var info ipInfoResult
			if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
				out <- lookupResult{ip: ip, err: err}
				return
			}
			out <- lookupResult{ip: ip, info: info}
		}(fn.Ip)
	}
	wg.Wait()
	close(out)

	for res := range out {
		if res.err != nil {
			log.Printf("IP info lookup failed for %s: %v", res.ip, res.err)
			continue
		}
		results[res.ip] = res.info
	}

	log.Printf("Successfully retrieved IP info for %d/%d addresses", len(results), len(*fullnodes))
	return results
}

func magicBytes(networkID int) []byte {
	msg := []byte(fmt.Sprintf("alephium-%d", networkID))
	hash := blake2b.Sum256(msg)
	var sum uint32
	for i := 0; i < len(hash); i += 4 {
		sum += binary.BigEndian.Uint32(hash[i : i+4])
	}
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, sum)
	return out
}

func djbHash(data []byte) uint32 {
	var h uint32 = 5381
	for _, b := range data {
		h = ((h << 5) + h) + uint32(b)
	}
	return h
}

func checksum(data []byte) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, djbHash(data))
	return out
}

func encodeCompactInt(n int64) ([]byte, error) {
	if n < 0 {
		return nil, fmt.Errorf("negative compact int unsupported: %d", n)
	}
	switch {
	case n < 0x20:
		return []byte{byte(n)}, nil
	case n < 0x2000:
		return []byte{0x40 | byte(n>>8), byte(n)}, nil
	case n < 0x20000000:
		return []byte{
			0x80 | byte(n>>24),
			byte(n >> 16),
			byte(n >> 8),
			byte(n),
		}, nil
	default:
		return []byte{
			0xC0 + 4,
			byte(n >> 24),
			byte(n >> 16),
			byte(n >> 8),
			byte(n),
		}, nil
	}
}

func decodeCompactInt(data []byte) (int, int, error) {
	if len(data) == 0 {
		return 0, 0, fmt.Errorf("empty compact int")
	}
	b0 := data[0]
	mode := b0 & 0xC0

	switch mode {
	case 0x00:
		return int(b0), 1, nil
	case 0x40:
		if len(data) < 2 {
			return 0, 0, fmt.Errorf("incomplete two-byte compact int")
		}
		v := int(b0&0x3F)<<8 | int(data[1])
		return v, 2, nil
	case 0x80:
		if len(data) < 4 {
			return 0, 0, fmt.Errorf("incomplete four-byte compact int")
		}
		v := int(b0&0x3F)<<24 | int(data[1])<<16 | int(data[2])<<8 | int(data[3])
		return v, 4, nil
	default:
		n := int(b0&0x3F) + 5
		if len(data) < n {
			return 0, 0, fmt.Errorf("incomplete multi-byte compact int")
		}
		if n != 5 {
			return 0, 0, fmt.Errorf("unsupported multi-byte compact int size: %d", n)
		}
		v := int(binary.BigEndian.Uint32(data[1:5]))
		return v, 5, nil
	}
}

func decodeLengthPrefixedBytes(data []byte) ([]byte, int, error) {
	length, used, err := decodeCompactInt(data)
	if err != nil {
		return nil, 0, err
	}
	if length < 0 || used+length > len(data) {
		return nil, 0, fmt.Errorf("invalid length-prefixed bytes")
	}
	return data[used : used+length], used + length, nil
}

func parseBrokerInfo(data []byte) (BrokerInfo, int, error) {
	if len(data) < cliqueIDLength {
		return BrokerInfo{}, 0, fmt.Errorf("broker info too short")
	}

	pos := 0
	cliqueID := make([]byte, cliqueIDLength)
	copy(cliqueID, data[pos:pos+cliqueIDLength])
	pos += cliqueIDLength

	brokerID, n, err := decodeCompactInt(data[pos:])
	if err != nil {
		return BrokerInfo{}, 0, err
	}
	pos += n

	brokerNum, n, err := decodeCompactInt(data[pos:])
	if err != nil {
		return BrokerInfo{}, 0, err
	}
	pos += n

	addrBytes, n, err := decodeLengthPrefixedBytes(data[pos:])
	if err != nil {
		return BrokerInfo{}, 0, err
	}
	pos += n

	port, n, err := decodeCompactInt(data[pos:])
	if err != nil {
		return BrokerInfo{}, 0, err
	}
	pos += n

	addr := net.IP(addrBytes).String()
	if len(addrBytes) != net.IPv4len && len(addrBytes) != net.IPv6len {
		addr = hex.EncodeToString(addrBytes)
	}

	return BrokerInfo{
		CliqueID:  cliqueID,
		BrokerID:  brokerID,
		BrokerNum: brokerNum,
		Address:   addr,
		Port:      port,
	}, pos, nil
}

func parseNeighborsPayload(payload []byte) []BrokerInfo {
	count, pos, err := decodeCompactInt(payload)
	if err != nil || count < 0 {
		return nil
	}

	results := make([]BrokerInfo, 0, count)
	rest := payload[pos:]
	for i := 0; i < count; i++ {
		item, used, parseErr := parseBrokerInfo(rest)
		if parseErr != nil {
			break
		}
		results = append(results, item)
		rest = rest[used:]
	}
	return results
}

func unwrapMessage(raw []byte, magic []byte) ([]byte, error) {
	if len(raw) < 12 {
		return nil, fmt.Errorf("frame too short")
	}
	if !bytes.Equal(raw[:4], magic) {
		return nil, fmt.Errorf("magic mismatch")
	}

	msgChecksum := raw[4:8]
	msgLen := int(binary.BigEndian.Uint32(raw[8:12]))
	if msgLen < 0 || len(raw) < 12+msgLen {
		return nil, fmt.Errorf("length mismatch")
	}

	data := raw[12 : 12+msgLen]
	if !bytes.Equal(checksum(data), msgChecksum) {
		return nil, fmt.Errorf("checksum mismatch")
	}
	return data, nil
}

func parseMessagePayloadTypeAndRest(data []byte) (int, []byte, error) {
	if len(data) < signatureLength+2 {
		return 0, nil, fmt.Errorf("data too short")
	}
	rest := data[signatureLength:]

	_, n, err := decodeCompactInt(rest)
	if err != nil {
		return 0, nil, err
	}
	rest = rest[n:]

	payloadType, n, err := decodeCompactInt(rest)
	if err != nil {
		return 0, nil, err
	}

	return payloadType, rest[n:], nil
}

func extractNeighborsFromMessage(raw []byte, networkID int) []BrokerInfo {
	data, err := unwrapMessage(raw, magicBytes(networkID))
	if err != nil {
		return nil
	}

	payloadType, payload, err := parseMessagePayloadTypeAndRest(data)
	if err != nil {
		return nil
	}
	if payloadType != codeNeighbors {
		return nil
	}

	return parseNeighborsPayload(payload)
}

func buildFindNodeMessage(networkID int, targetCliqueID []byte) ([]byte, error) {
	if len(targetCliqueID) != cliqueIDLength {
		return nil, fmt.Errorf("invalid target clique id length: %d", len(targetCliqueID))
	}

	signature := make([]byte, signatureLength)
	header, err := encodeCompactInt(discoveryVersion)
	if err != nil {
		return nil, err
	}
	payloadType, err := encodeCompactInt(codeFindNode)
	if err != nil {
		return nil, err
	}

	data := append(signature, header...)
	data = append(data, payloadType...)
	data = append(data, targetCliqueID...)

	frame := make([]byte, 0, 12+len(data))
	frame = append(frame, magicBytes(networkID)...)
	frame = append(frame, checksum(data)...)

	length := make([]byte, 4)
	binary.BigEndian.PutUint32(length, uint32(len(data)))
	frame = append(frame, length...)
	frame = append(frame, data...)
	return frame, nil
}

func buildPingMessage(networkID int) ([]byte, error) {
	signature := make([]byte, signatureLength)
	header, err := encodeCompactInt(discoveryVersion)
	if err != nil {
		return nil, err
	}
	payloadType, err := encodeCompactInt(codePing)
	if err != nil {
		return nil, err
	}

	sessionID := make([]byte, sessionIDLength)
	_, _ = crand.Read(sessionID)

	optionNone := []byte{0x00}

	data := append(signature, header...)
	data = append(data, payloadType...)
	data = append(data, sessionID...)
	data = append(data, optionNone...)

	frame := make([]byte, 0, 12+len(data))
	frame = append(frame, magicBytes(networkID)...)
	frame = append(frame, checksum(data)...)

	length := make([]byte, 4)
	binary.BigEndian.PutUint32(length, uint32(len(data)))
	frame = append(frame, length...)
	frame = append(frame, data...)
	return frame, nil
}

func pingNode(endpoint string, networkID int, timeout time.Duration) bool {
	conn, err := net.Dial("udp", endpoint)
	if err != nil {
		return false
	}
	defer conn.Close()

	msg, err := buildPingMessage(networkID)
	if err != nil {
		return false
	}
	if _, err := conn.Write(msg); err != nil {
		return false
	}

	magic := magicBytes(networkID)
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 65535)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return false
		}
		data, unwrapErr := unwrapMessage(buf[:n], magic)
		if unwrapErr != nil {
			continue
		}
		payloadType, _, parseErr := parseMessagePayloadTypeAndRest(data)
		if parseErr != nil {
			continue
		}
		if payloadType == codePong {
			return true
		}
	}
}

func parseTCPHelloClientID(data []byte) (string, error) {
	pos := 0
	_, n, err := decodeCompactInt(data[pos:])
	if err != nil {
		return "", err
	}
	pos += n

	code, n, err := decodeCompactInt(data[pos:])
	if err != nil {
		return "", err
	}
	pos += n
	if code != codeHello {
		return "", fmt.Errorf("not hello payload")
	}

	size, n, err := decodeCompactInt(data[pos:])
	if err != nil {
		return "", err
	}
	pos += n
	if size < 0 || size > 256 || pos+size > len(data) {
		return "", fmt.Errorf("invalid clientId size")
	}

	return string(data[pos : pos+size]), nil
}

func fetchClientVersionTCP(host string, port int, networkID int, timeout time.Duration) string {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	if err != nil {
		return ""
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))

	header := make([]byte, 12)
	if _, err := io.ReadFull(conn, header); err != nil {
		return ""
	}

	msgLen := int(binary.BigEndian.Uint32(header[8:12]))
	if msgLen <= 0 || msgLen > 1024*1024 {
		return ""
	}

	body := make([]byte, msgLen)
	if _, err := io.ReadFull(conn, body); err != nil {
		return ""
	}

	raw := append(header, body...)
	data, err := unwrapMessage(raw, magicBytes(networkID))
	if err != nil {
		return ""
	}

	clientID, err := parseTCPHelloClientID(data)
	if err != nil {
		return ""
	}
	return clientID
}
