package neo4jstore

import "time"

// Cypher returns numeric properties as int64 by default; sometimes
// the bolt driver hands them back as int64 even when stored as int.
// These helpers tolerate both shapes plus the empty-property zero
// value so decoders are robust to future schema changes.

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asBool(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

func asInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return 0
}

func asInt64(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	case float64:
		return int64(x)
	}
	return 0
}

func asFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int64:
		return float64(x)
	case int:
		return float64(x)
	}
	return 0
}

func asUnixMs(v any) time.Time {
	ms := asInt64(v)
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
