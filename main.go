package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-co-op/gocron"
	"github.com/ipinfo/go/ipinfo/cache"
	"github.com/ipinfo/go/v2/ipinfo"
	"github.com/joho/godotenv"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type FullnodeDb struct {
	gorm.Model
	CliqueId          string `gorm:"unique"`
	BrokerId          uint
	GroupNumPerBroker uint
	Ip                string
	Port              uint
	ClientVersion     string
	IsSynced          bool
	Hostname          string
	City              string
	Region            string
	Country           string
	Location          string
	Org               string
	Postal            string
	Timezone          string
}

type Address struct {
	Addr string
	Port uint
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

func main() {

	err := godotenv.Load(".env")
	if err != nil {
		fmt.Printf("Error load env, %s\n", err)
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
	db.AutoMigrate(&FullnodeDb{})
	fmt.Printf("Starting, running every %s\n", cronUpdate)
	fmt.Printf("Querying %s\n", fullnodesList)
	s := gocron.NewScheduler(time.UTC)
	s.Every(cronUpdate).Do(updateFullnodeList, fullnodesList)
	s.StartBlocking()

}

func updateFullnodeList(fullnodesList []string) {
	fmt.Println("Update fullnodes")
	fullnodes, err := getFullnodes(fullnodesList)
	if err != nil {
		fmt.Printf("Error get fullnodes, %s", err)
	}

	result := db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "clique_id"}},
		DoUpdates: clause.Assignments(map[string]interface{}{"updated_at": time.Now()})}).Create(&fullnodes)

	if result.Error != nil {
		log.Fatalf("Error insert fullnodes, %s", result.Error)
	}

	var emptyIpFullnodes []FullnodeDb
	resultEmtpy := db.Where("location = ?", "").Find(&emptyIpFullnodes)

	//`only update existing fullnode
	if resultEmtpy.RowsAffected > 0 {
		ipInfo := getIpInfo(&fullnodes)
		for k, v := range ipInfo {
			db.Model(&FullnodeDb{}).Where("ip = ?", k).Updates(FullnodeDb{
				Hostname: v.Hostname,
				City:     v.City,
				Region:   v.Region,
				Country:  v.Country,
				Location: v.Location,
				Org:      v.Org,
				Postal:   v.Postal,
				Timezone: v.Timezone,
			})
		}
	}

}

func getJSON(url string) ([]Fullnode, error) {
	var fullnode []Fullnode
	resp, err := http.Get(url)
	if err != nil {
		return []Fullnode{}, fmt.Errorf("cannot fetch URL %q: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return []Fullnode{}, fmt.Errorf("unexpected http GET status: %s", resp.Status)
	}
	// We could check the resulting content type
	// here if desired.
	err = json.NewDecoder(resp.Body).Decode(&fullnode)
	if err != nil {
		return fullnode, fmt.Errorf("cannot decode JSON: %v", err)
	}
	return fullnode, nil
}

// query endpoint infos/inter-clique-peer-info
func getFullnodes(nodesToQuery []string) ([]FullnodeDb, error) {

	var fullnode []Fullnode
	for _, node := range nodesToQuery {
		url := fmt.Sprintf("%s/%s", node, API_PEERS_ENDPOINT)

		fullnodeListResult, err := getJSON(url)
		if err != nil {
			fmt.Printf("Error in getting fullnodes peers, %s", err)
		}

		fullnode = append(fullnodeListResult, fullnodeListResult...)
	}

	//fmt.Printf("%v+", fullnode)

	var fullnodeDb []FullnodeDb

	for _, item := range fullnode {
		fullnodeDb = appendIfNotExists(fullnodeDb, FullnodeDb{
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

func getIpInfo(fullnodes *[]FullnodeDb) ipinfo.BatchCore {
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
			BatchSize:       2,
			TimeoutPerBatch: 0,
			TimeoutTotal:    5,
		},
	)
	if err != nil {
		fmt.Printf("Error getting ipinfo, %s", err)
		return ipinfo.BatchCore{}
	}

	return batchResult
}

// Function to check if the struct with specific properties exists in the slice
func contains(slice []FullnodeDb, ip string, port uint) bool {
	for _, item := range slice {
		if item.Ip == ip && item.Port == port {
			return true
		}
	}
	return false
}

// Function to append a struct to the slice if it doesn't already exist
func appendIfNotExists(slice []FullnodeDb, newItem FullnodeDb) []FullnodeDb {
	if !contains(slice, newItem.Ip, newItem.Port) {
		slice = append(slice, newItem)
	}
	return slice
}
