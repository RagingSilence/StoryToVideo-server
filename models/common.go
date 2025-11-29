package models

import (
	"database/sql/driver"
	"encoding/json"
)

// JSONMap 用于处理数据库中的 JSON 字段 (GORM 兼容)
type JSONMap map[string]interface{}

// Value 实现 Gorm 的 Valuer 接口（写入数据库）
func (j JSONMap) Value() (driver.Value, error) {
	if j == nil {
		return "{}", nil
	}
	return json.Marshal(j)
}

// Scan 实现 Gorm 的 Scanner 接口（读取数据库）
func (j *JSONMap) Scan(value interface{}) error {
	if value == nil {
		*j = make(JSONMap)
		return nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		return nil
	}
	return json.Unmarshal(bytes, j)
}