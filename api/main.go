package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	_ "main/docs"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	mapmodels "github.com/notrustverify/alephium-fullnodes-map"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type NumNodesDb struct {
	Count     int        `json:"count"`
	CreatedAt *time.Time `json:"createdAt"`
}

type FullnodeApi struct {
	ClientVersion string     `json:"clientVersion"`
	IsSynced      bool       `json:"isSynced"`
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
	ClientVersion string `json:"clientVersion"`
	Count         int    `json:"count"`
}

type SyncCount struct {
	IsSynced bool `json:"isSynced"`
	Count    int  `json:"count"`
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
	router.GET("/syncstatus", getSyncedStatus)
	router.GET("/historic", getNumNodes)

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
// @Param upperBound query string false "Upper bound in hours, default 1"
// @Param lowerBound query string false "Lower bound in hours, default 4"
func getFullnodes(c *gin.Context) {
	var fullnodes []FullnodeApi
	timeNow := time.Now()

	// Get upper and lower bounds from query parameters
	upperBoundParam := c.DefaultQuery("upperBound", "1")
	lowerBoundParam := c.DefaultQuery("lowerBound", "4")

	upperBound, err := strconv.Atoi(upperBoundParam)
	if err != nil {
		log.Printf("Error parsing upperBound parameter: %v (using default value 1)", err)
		upperBound = 1
	}

	lowerBound, err := strconv.Atoi(lowerBoundParam)
	if err != nil {
		log.Printf("Error parsing lowerBound parameter: %v (using default value 4)", err)
		lowerBound = 4
	}

	// Ensure lower bound is greater than upper bound
	if upperBound >= lowerBound {
		log.Printf("Warning: upperBound (%d) must be less than lowerBound (%d), adjusting to defaults", upperBound, lowerBound)
		upperBound = 1
		lowerBound = 4
	}

	timeUpperBound := timeNow.Add(-time.Hour * time.Duration(upperBound))
	timeLowerBound := timeNow.Add(-time.Hour * time.Duration(lowerBound))

	log.Printf("Fetching fullnodes updated between %v and %v (%d to %d hours ago)",
		timeLowerBound.Format(time.RFC3339), timeUpperBound.Format(time.RFC3339), lowerBound, upperBound)

	result := dbHandler.Model(&mapmodels.FullnodeDb{}).
		Where("updated_at > ? AND updated_at < ? AND location != ''", timeLowerBound, timeUpperBound).
		Find(&fullnodes)

	if result.Error != nil {
		log.Printf("Error getting fullnodes from database: %v", result.Error)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch fullnodes"})
		return
	}

	// Get total count without the location filter for comparison
	var totalCount int64
	dbHandler.Model(&mapmodels.FullnodeDb{}).
		Where("updated_at > ? AND updated_at < ?", timeLowerBound, timeUpperBound).
		Count(&totalCount)

	log.Printf("Query stats: Found %d fullnodes with location out of %d total fullnodes in the time window",
		result.RowsAffected, totalCount)

	if result.RowsAffected == 0 {
		if totalCount > 0 {
			log.Printf("Warning: Found %d fullnodes but none have location data set", totalCount)
		} else {
			log.Printf("No fullnodes found in the database updated in the specified time window")
		}
	} else {
		log.Printf("Fullnode locations found: %d records", len(fullnodes))
	}

	c.JSON(http.StatusOK, fullnodes)
}

// GetVersion godoc
// @Summary Get version run by fullnodes peers
// @Tags fullnodes
// @Produce json
// @Success 200 {array} ClientVersionCount
// @Router /versions [get]
func getVersions(c *gin.Context) {

	var countVersion []ClientVersionCount
	result := dbHandler.Model(&mapmodels.FullnodeDb{}).Distinct("ip", "port").Select("client_version, COUNT(*) as count").Group("client_version").Order("count").Scan(&countVersion)

	if result.RowsAffected > 0 && result.Error == nil {
		c.JSON(http.StatusOK, countVersion)
	} else {
		log.Printf("Error getting count: %s\n", result.Error)
		c.JSON(http.StatusOK, make([]string, 0))
	}
}

// SyncStatus godoc
// @Summary Get number of fullnodes synced and not synced
// @Tags fullnodes
// @Produce json
// @Success 200 {array} SyncCount
// @Router /syncstatus [get]
func getSyncedStatus(c *gin.Context) {

	var countSync []SyncCount
	result := dbHandler.Model(&mapmodels.FullnodeDb{}).Distinct("ip", "port").Select("is_synced, COUNT(*) as count").Group("is_synced").Order("count").Scan(&countSync)

	if result.RowsAffected > 0 && result.Error == nil {
		c.JSON(http.StatusOK, countSync)
	} else {
		log.Printf("Error getting count: %s\n", result.Error)
		c.JSON(http.StatusOK, make([]string, 0))
	}
}

// GetNumNodes godoc
// @Summary Return number of nodes connected historically
// @Tags fullnodes
// @Produce json
// @Success 200 {array} NumNodesDb
// @Router /historic [get]
// @Param limt query string false "Limit number of historic data, default 50"
func getNumNodes(c *gin.Context) {
	var countSync []NumNodesDb

	limit := c.DefaultQuery("limit", "50")
	limitToInt, err := strconv.Atoi(limit)
	if err != nil {
		log.Printf("Error with parameters, %s", err)
		limitToInt = 50
	}

	result := dbHandler.Model(&NumNodesDb{}).Order("updated_at DESC").Limit(limitToInt).Find(&countSync)

	if result.RowsAffected > 0 && result.Error == nil {
		c.JSON(http.StatusOK, countSync)
	} else {
		log.Printf("Error getting count: %s\n", result.Error)
		c.JSON(http.StatusOK, make([]string, 0))
	}
}
