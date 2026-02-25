package main

import (
	"bytes"
	crand "crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-co-op/gocron"
	"github.com/ipinfo/go/ipinfo/cache"
	"github.com/ipinfo/go/v2/ipinfo"
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
)

const (
	signatureLength         = 64
	cliqueIDLength          = 33
	discoveryVersion  int64 = 65536
	codeFindNode            = 2
	codeNeighbors           = 3
	codeHello               = 0
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

	fullnodes, err := getFullnodes(fullnodesList, networkID, discoveryDepth)
	if err != nil {
		log.Printf("Failed to get fullnodes data: %v", err)
	}

	numNodes := NumNodesDb{Count: len(fullnodes)}
	if result := db.Create(&numNodes); result.Error != nil {
		log.Printf("Failed to update node count in database: %v", result.Error)
	}
	if len(fullnodes) == 0 {
		log.Printf("No peers discovered; skipping fullnode upsert (check UDP reachability of seeds)")
		return
	}

	// Store nodes by ip:port identity (not nodeId/cliqueId).
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
	log.Printf("Successfully updated %d fullnode records", result.RowsAffected)

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
				Location:    v.Location,
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

func getFullnodes(seedNodes []string, networkID int, discoveryDepth int) ([]mapmodels.FullnodeDb, error) {
	log.Printf("Running discovery with %d seeds, depth=%d", len(seedNodes), discoveryDepth)
	neighbors := discoverNeighbors(seedNodes, networkID, discoveryDepth)
	log.Printf("Discovered %d unique peers via UDP neighbors", len(neighbors))

	versions := fetchClientVersions(neighbors, networkID)

	fullnodes := make([]mapmodels.FullnodeDb, 0, len(neighbors))
	for endpoint, node := range neighbors {
		if node.Address == "" || node.Port <= 0 || node.Port > 65535 {
			log.Printf("Skipping invalid peer endpoint=%s", endpoint)
			continue
		}

		fullnodes = append(fullnodes, mapmodels.FullnodeDb{
			CliqueId:          hex.EncodeToString(node.CliqueID),
			BrokerId:          uint(node.BrokerID),
			GroupNumPerBroker: uint(node.BrokerNum),
			Ip:                node.Address,
			Port:              uint(node.Port),
			IsSynced:          false,
			ClientVersion:     versions[endpoint],
		})
	}

	return fullnodes, nil
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

func queryNeighbors(endpoint string, networkID int, timeout time.Duration) ([]BrokerInfo, error) {
	conn, err := net.Dial("udp", endpoint)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	target := make([]byte, cliqueIDLength)
	_, _ = crand.Read(target)
	target[0] = 0x02 // valid compressed pubkey prefix family

	msg, err := buildFindNodeMessage(networkID, target)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(msg); err != nil {
		return nil, err
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

func getIpInfo(fullnodes *[]mapmodels.FullnodeDb) ipinfo.BatchCore {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Recovered from ipinfo batch panic: %v", r)
		}
	}()

	if len(*fullnodes) == 0 {
		log.Printf("Warning: No fullnodes provided for IP info lookup")
		return ipinfo.BatchCore{}
	}
	if strings.TrimSpace(IPINFO_TOKEN) == "" {
		log.Printf("IPINFO_TOKEN is empty; skipping location enrichment")
		return ipinfo.BatchCore{}
	}

	client := ipinfo.NewClient(
		nil,
		ipinfo.NewCache(cache.NewInMemory().WithExpiration(5*time.Minute)),
		IPINFO_TOKEN,
	)

	var ips []string
	for _, fn := range *fullnodes {
		ips = append(ips, fn.Ip)
	}

	batchResult, err := client.GetIPStrInfoBatch(ips,
		ipinfo.BatchReqOpts{
			BatchSize:       30,
			TimeoutPerBatch: 0,
			TimeoutTotal:    5,
		},
	)
	if err != nil {
		log.Printf("Failed to get IP info batch: %v", err)
		return ipinfo.BatchCore{}
	}

	if len(batchResult) == 0 {
		log.Printf("Warning: IP info batch request returned no results for %d IPs", len(ips))
	} else {
		log.Printf("Successfully retrieved IP info for %d/%d addresses", len(batchResult), len(ips))
	}

	return batchResult
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
