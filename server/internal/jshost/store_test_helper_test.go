package jshost

import (
	"github.com/venera-app/venera-nas/internal/model"
)

// memStore 是测试用的内存版 SourceDataStore。
type memStore struct {
	data    map[string]map[string]any
	cookies map[string][]model.CookieRecord
}

func newMemStore() *memStore {
	return &memStore{
		data:    map[string]map[string]any{},
		cookies: map[string][]model.CookieRecord{},
	}
}

func (m *memStore) GetSourceData(key string) (map[string]any, error) {
	if d, ok := m.data[key]; ok {
		return d, nil
	}
	return map[string]any{}, nil
}

func (m *memStore) MergeSourceData(key string, patch map[string]any) error {
	if m.data[key] == nil {
		m.data[key] = map[string]any{}
	}
	for k, v := range patch {
		m.data[key][k] = v
	}
	return nil
}

func (m *memStore) GetCookies(key string) ([]model.CookieRecord, error) {
	return m.cookies[key], nil
}

func (m *memStore) PutCookies(key string, c []model.CookieRecord) error {
	m.cookies[key] = append(m.cookies[key], c...)
	return nil
}
