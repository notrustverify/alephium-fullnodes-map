package mapmodels

import "gorm.io/gorm"

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
