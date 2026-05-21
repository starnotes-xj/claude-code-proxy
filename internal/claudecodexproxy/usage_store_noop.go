//go:build !sqlite

package claudecodexproxy

func newUsageStore(_ string) (usageStore, error) {
	return &noopUsageStore{}, nil
}
