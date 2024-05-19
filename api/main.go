package main

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	_ "main/docs"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type FullnodeDb struct {
	gorm.Model
	CliqueId          string `json:"cliqueid"`
	BrokerId          uint   `json:"brokerid"`
	GroupNumPerBroker uint   `json:"groupNumPerBroker"`
	Ip                string `json:"ip"`
	Port              uint   `json:"port"`
	ClientVersion     string `json:"clientVersion"`
	IsSynced          bool   `json:"isSynced"`
	Hostname          string `json:"hostname"`
	City              string `json:"city"`
	Region            string `json:"region"`
	Country           string `json:"country"`
	Location          string `json:"location"`
	Org               string `json:"org"`
	Postal            string `json:"postal"`
	Timezone          string `json:"timezone"`
}

type FullnodeApi struct {
	Ip            string     `json:"ip"`
	ClientVersion string     `json:"clientVersion"`
	IsSynced      bool       `json:"isSynced"`
	Hostname      string     `json:"hostname"`
	City          string     `json:"city"`
	Region        string     `json:"region"`
	Country       string     `json:"country"`
	Location      string     `json:"location"`
	Org           string     `json:"org"`
	Postal        string     `json:"postal"`
	Timezone      string     `json:"timezone"`
	UpdatedAt     *time.Time `json:"updatedAt"`
}

type ClientVersionCount struct {
	ClientVersion string `json:"client_version"`
	Count         int    `json:"count"`
}

var dbHandler *gorm.DB

// @title Fullnodes Aggregator API
// @version 1.0
// @description Find connected fullnodes peers and get their approximate location
// @host map.alephium.notrustverify.ch
// @schemes https
// @BasePath /
func main() {
	err := godotenv.Load(".env")
	if err != nil {
		fmt.Println("No env file, will use system variable")
	}

	// get and set mode from env file for gin
	mode := os.Getenv("GIN_MODE")
	corsConfig := cors.Config{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"GET", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type"},
		AllowCredentials: true,
	}

	dbPath := os.Getenv("DB_PATH")

	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		panic("failed to connect database")
	}
	dbHandler = db

	gin.SetMode(mode)
	router := gin.Default()
	router.Use(cors.New(corsConfig))
	router.GET("/fullnodes", getFullnodes)
	router.GET("/versions", getVersions)

	// redict to index.html
	router.GET("/docs", func(c *gin.Context) {
		c.Redirect(http.StatusMovedPermanently, "/docs/index.html")
	})
	router.GET("/docs/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))

	router.Run("0.0.0.0:8080")

}

// GetFullnodes godoc
// @Summary Get detected fullnodes peers
// @Tags fullnodes
// @Produce json
// @Success 200 {array} FullnodeApi
// @Router /fullnodes [get]
func getFullnodes(c *gin.Context) {

	var fullnodes []FullnodeApi
	timeNow := time.Now()
	lastTimeUpdatedParam := c.DefaultQuery("lastUpdate", "6")
	lastTimeUpdated, err := strconv.Atoi(lastTimeUpdatedParam)
	if err != nil {
		fmt.Printf("Error with parameters, %s", err)
		lastTimeUpdated = 6
	}

	result := dbHandler.Model(&FullnodeDb{}).Where("updated_at > ?", timeNow.Add(time.Hour*time.Duration(-lastTimeUpdated))).Find(&fullnodes)

	if result.RowsAffected > 0 && result.Error == nil {
		c.JSON(http.StatusOK, fullnodes)
	} else {
		fmt.Printf("Error getting fullnodes: %s\n", result.Error)
		c.JSON(http.StatusOK, make([]string, 0))
	}
}

// GetVersion godoc
// @Summary Get version run by fullnodes peers
// @Tags fullnodes
// @Produce json
// @Success 200 {array} ClientVersionCount
// @Router /versions [get]
func getVersions(c *gin.Context) {

	var countVersion []ClientVersionCount
	result := dbHandler.Model(&FullnodeDb{}).Distinct("ip", "port").Select("client_version, COUNT(*) as count").Group("client_version").Order("count").Scan(&countVersion)

	if result.RowsAffected > 0 && result.Error == nil {
		c.JSON(http.StatusOK, countVersion)
	} else {
		fmt.Printf("Error getting count: %s\n", result.Error)
		c.JSON(http.StatusOK, make([]string, 0))
	}
}
