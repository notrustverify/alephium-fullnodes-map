package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-co-op/gocron"
	"github.com/ipinfo/go/ipinfo/cache"
	"github.com/ipinfo/go/v2/ipinfo"
	"github.com/joho/godotenv"
	mapmodels "github.com/notrustverify/alephium-fullnodes-map"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type NumNodesDb struct {
	gorm.Model
	Count int
}

type IP struct {
	Query string
}

type Address struct {
	Addr string
	Port uint
}

type SelfNode struct {
	BuildInfo struct {
		ReleaseVersion string `json:"releaseVersion"`
		Commit         string `json:"commit"`
	} `json:"buildInfo"`
	Upnp            bool        `json:"upnp"`
	ExternalAddress interface{} `json:"externalAddress"`
}

type SelfClique struct {
	CliqueID string `json:"cliqueId"`
	Nodes    []struct {
		Address      string `json:"address"`
		RestPort     int    `json:"restPort"`
		WsPort       int    `json:"wsPort"`
		MinerAPIPort int    `json:"minerApiPort"`
	} `json:"nodes"`
	SelfReady bool `json:"selfReady"`
	Synced    bool `json:"synced"`
}

type SelfVersion struct {
	Version string `json:"version"`
}

type SelfChainParams struct {
	NetworkID             int `json:"networkId"`
	NumZerosAtLeastInHash int `json:"numZerosAtLeastInHash"`
	GroupNumPerBroker     int `json:"groupNumPerBroker"`
	Groups                int `json:"groups"`
}

type Fullnode struct {
	CliqueId          string
	BrokerId          uint
	GroupNumPerBroker uint
	Address           Address
	Port              uint
	ClientVersion     string
	IsSynced          bool
}

var IPINFO_TOKEN string
var db *gorm.DB

const API_PEERS_ENDPOINT = "infos/inter-clique-peer-info"
const API_SELF_VERSION_ENDPOINT = "infos/version"
const API_SELF_CHAIN_PARAM_ENDPOINT = "infos/chain-params"
const API_SELF_CLIQUE_ENDPOINT = "infos/self-clique"

const ONE_WEEK_HOUR = 168

func main() {

	err := godotenv.Load(".env")
	if err != nil {
		log.Printf("Error load env, %s\n", err)
	}
	dbPath := os.Getenv("DB_PATH")
	IPINFO_TOKEN = os.Getenv("IPINFO_TOKEN")
	cronUpdate := os.Getenv("CRON_INTERVAL")
	fullnodesList := strings.Split(os.Getenv("FULLNODE_LIST"), ",")

	if len(fullnodesList) <= 0 {
		log.Fatalf("Fullnodes list to query is empty\n")
		os.Exit(1)
	}

	conn, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		panic("failed to connect database\n")
	}
	db = conn

	// Migrate the schema
	db.AutoMigrate(&mapmodels.FullnodeDb{}, &NumNodesDb{})
	log.Printf("Starting, running every %s\n", cronUpdate)
	log.Printf("Querying %s\n", fullnodesList)

	s := gocron.NewScheduler(time.UTC)
	s.Every(cronUpdate).Do(updateFullnodeList, fullnodesList)
	s.StartBlocking()

}

func updateFullnodeList(fullnodesList []string) {
	log.Println("Update fullnodes")

	fullnodes, err := getFullnodes(fullnodesList)
	if err != nil {
		log.Printf("Error get fullnodes, %s", err)
	}

	// update the actual number of nodes find
	numNodes := NumNodesDb{Count: len(fullnodes)}
	db.Create(&numNodes)

	// update existing nodes based on their clique id
	result := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "ip"}, {Name: "port"}},
		DoUpdates: clause.AssignmentColumns([]string{"client_version", "is_synced", "group_num_per_broker", "port", "broker_id", "updated_at"}),
	}).Create(&fullnodes)

	if result.Error != nil {
		log.Fatalf("Error insert fullnodes, %s", result.Error)
	}

	var emptyIpFullnodes []mapmodels.FullnodeDb
	resultEmtpy := db.Where("location = ? OR ip_updated_at < ? OR ip_updated_at is NULL", "", time.Now().Add(-(time.Hour * ONE_WEEK_HOUR))).Find(&emptyIpFullnodes)

	//`only update existing fullnode
	if resultEmtpy.RowsAffected > 0 {
		ipInfo := getIpInfo(&emptyIpFullnodes)

		for k, v := range ipInfo {
			db.Model(&mapmodels.FullnodeDb{}).Where("ip = ?", k).Updates(mapmodels.FullnodeDb{
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
		}
	}

	log.Printf("Update done")

}

// retrieve info from queried node
func getSelfInfo(basePath string) (Fullnode, error) {
	selfVersionUrl := fmt.Sprintf("%s/%s", basePath, API_SELF_VERSION_ENDPOINT)
	selfChainParamsUrl := fmt.Sprintf("%s/%s", basePath, API_SELF_CHAIN_PARAM_ENDPOINT)
	selfCliqueUrl := fmt.Sprintf("%s/%s", basePath, API_SELF_CLIQUE_ENDPOINT)

	var selfFullnode Fullnode
	resultVersion, err := getJSONNotArray[SelfVersion](selfVersionUrl)
	if err != nil {
		return Fullnode{}, fmt.Errorf("error with self version: %s", err)
	}

	resultChainParam, err := getJSONNotArray[SelfChainParams](selfChainParamsUrl)
	if err != nil {
		return Fullnode{}, fmt.Errorf("error with chain param: %s", err)
	}

	resultSelfClique, err := getJSONNotArray[SelfClique](selfCliqueUrl)
	if err != nil {
		return Fullnode{}, fmt.Errorf("error with self clique: %s", err)
	}

	hostname := strings.Split(basePath, "://")[1]
	publicIp, err := getPublicIp(hostname)
	if err != nil {
		return Fullnode{}, fmt.Errorf("cannot get public ip, %s", publicIp)
	}

	selfFullnode.ClientVersion = fmt.Sprintf("scala-alephium/%s/Linux", resultVersion.Version)
	selfFullnode.GroupNumPerBroker = uint(resultChainParam.GroupNumPerBroker)
	selfFullnode.CliqueId = resultSelfClique.CliqueID
	selfFullnode.IsSynced = resultSelfClique.Synced
	selfFullnode.Address.Addr = publicIp
	selfFullnode.Address.Port = 9973

	return selfFullnode, nil
}

func getPublicIp(host string) (string, error) {
	var hostClean = host

	apiSplitKey := strings.Split(host, "@")
	if len(apiSplitKey) > 1 {
		hostClean = apiSplitKey[1]
	}

	portSplit := strings.Split(hostClean, ":")
	if len(portSplit) > 1 {
		hostClean = portSplit[0]
	}

	ips, err := net.LookupIP(hostClean)
	if err != nil {
		return "", err
	}

	for _, ip := range ips {
		if ipv4 := ip.To4(); ipv4 != nil {
			return ipv4.To4().String(), nil
		}

	}

	return ips[0].String(), err

}

func getJSON[T any](url string) ([]T, error) {
	var fullnode []T

	req := createHttpRequest(url)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return []T{}, fmt.Errorf("cannot fetch URL %q: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return []T{}, fmt.Errorf("unexpected http GET status: %s", resp.Status)
	}
	// We could check the resulting content type
	// here if desired.
	err = json.NewDecoder(resp.Body).Decode(&fullnode)
	if err != nil {
		return fullnode, fmt.Errorf("cannot decode JSON: %v", err)
	}

	return fullnode, nil
}

func getJSONNotArray[T any](url string) (T, error) {
	var fullnode T

	req := createHttpRequest(url)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fullnode, fmt.Errorf("cannot fetch URL %q: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fullnode, fmt.Errorf("unexpected http GET status: %s", resp.Status)
	}
	// We could check the resulting content type
	// here if desired.
	err = json.NewDecoder(resp.Body).Decode(&fullnode)
	if err != nil {
		return fullnode, fmt.Errorf("cannot decode JSON: %v", err)
	}
	return fullnode, nil
}

func createHttpRequest(uri string) *http.Request {
	var req *http.Request

	apiSplitKey := strings.Split(uri, "@")
	if len(apiSplitKey) > 1 {
		req, _ = http.NewRequest("GET", apiSplitKey[1], nil)
		req.Header.Set("X-API-KEY", apiSplitKey[0])
	} else {
		req, _ = http.NewRequest("GET", uri, nil)
	}

	return req
}

// query endpoint infos/inter-clique-peer-info
func getFullnodes(nodesToQuery []string) ([]mapmodels.FullnodeDb, error) {
	var fullnodes []Fullnode
	var checkNodesToQuery []string // only use fullnodes that are reachable

	for _, node := range nodesToQuery {

		selfNode, err := getSelfInfo(node)
		// if issue when querying fullnode dont include it later
		if err != nil {
			log.Printf("Error with node %s, err: %s", node, err)
			continue
		}

		fullnodes = append(fullnodes, selfNode)
		checkNodesToQuery = append(checkNodesToQuery, node)

	}

	wg := sync.WaitGroup{}
	fullnodeListResult := make([][]Fullnode, len(checkNodesToQuery))

	for i, node := range checkNodesToQuery {
		wg.Add(1)

		go func(id int) {
			url := fmt.Sprintf("%s/%s", node, API_PEERS_ENDPOINT)

			defer wg.Done()
			result, err := getJSON[Fullnode](url)

			if err != nil {
				log.Printf("Error in getting fullnodes peers, %s", err)
				return
			}
			fullnodeListResult[id] = result
		}(i)
		wg.Wait()

		fullnodes = append(fullnodes, fullnodeListResult[i]...)
	}
	var fullnodeDb []mapmodels.FullnodeDb

	for _, item := range fullnodes {

		fullnodeDb = appendIfNotExists(fullnodeDb, mapmodels.FullnodeDb{
			CliqueId:          item.CliqueId,
			BrokerId:          item.BrokerId,
			GroupNumPerBroker: item.GroupNumPerBroker,
			Ip:                item.Address.Addr,
			Port:              item.Address.Port,
			IsSynced:          item.IsSynced,
			ClientVersion:     item.ClientVersion,
		})

	}

	return fullnodeDb, nil

}

func getIpInfo(fullnodes *[]mapmodels.FullnodeDb) ipinfo.BatchCore {
	client := ipinfo.NewClient(
		nil,
		ipinfo.NewCache(cache.NewInMemory().WithExpiration(5*time.Minute)),
		IPINFO_TOKEN,
	)

	var ips []string
	for _, fn := range *fullnodes {
		ips = append(ips, fn.Ip)

	}

	// batchResult will contain all the batch lookup data
	batchResult, err := client.GetIPStrInfoBatch(ips,
		ipinfo.BatchReqOpts{
			BatchSize:       30,
			TimeoutPerBatch: 0,
			TimeoutTotal:    5,
		},
	)
	if err != nil {
		log.Printf("Error getting ipinfo, %s", err)
		return ipinfo.BatchCore{}
	}

	return batchResult
}

// Function to check if the struct with specific properties exists in the slice
func contains(slice []mapmodels.FullnodeDb, ip string, port uint) bool {
	for _, item := range slice {
		if item.Ip == ip && item.Port == port {
			return true
		}
	}
	return false
}

// Function to append a struct to the slice if it doesn't already exist
func appendIfNotExists(slice []mapmodels.FullnodeDb, newItem mapmodels.FullnodeDb) []mapmodels.FullnodeDb {
	if !contains(slice, newItem.Ip, newItem.Port) {
		slice = append(slice, newItem)
	}
	return slice
}
