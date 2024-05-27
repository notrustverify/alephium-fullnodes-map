package mapmodels

import (
	"time"
)

type FullnodeDb struct {
	CliqueId          string `gorm:"unique"`
	BrokerId          uint
	GroupNumPerBroker uint
	Ip                string `gorm:"primaryKey"`
	Port              uint   `gorm:"primaryKey"`
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
	IpUpdatedAt       time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
	DeletedAt         time.Time `gorm:"index"`
}
