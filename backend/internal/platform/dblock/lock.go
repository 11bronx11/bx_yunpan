package dblock

import "gorm.io/gorm"

func Transaction(tx *gorm.DB, key string) error {
	return tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", key).Error
}
