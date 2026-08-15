package bytesize

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

var units = map[string]float64{
	"":    1,
	"b":   1,
	"kb":  1_000,
	"mb":  1_000_000,
	"gb":  1_000_000_000,
	"tb":  1_000_000_000_000,
	"kib": 1 << 10,
	"mib": 1 << 20,
	"gib": 1 << 30,
	"tib": 1 << 40,
}

// Parse converts values such as "512MiB", "1.5GB", and "4096" to bytes.
func Parse(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("empty byte size")
	}

	i := 0
	for i < len(s) && ((s[i] >= '0' && s[i] <= '9') || s[i] == '.') {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("invalid byte size %q", s)
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid byte size %q", s)
	}
	mult, ok := units[strings.TrimSpace(s[i:])]
	if !ok {
		return 0, fmt.Errorf("unsupported byte-size unit in %q", s)
	}
	v := n * mult
	if v > math.MaxInt64 {
		return 0, fmt.Errorf("byte size %q overflows int64", s)
	}
	return int64(v), nil
}

func Format(n int64) string {
	if n < 0 {
		return "-" + Format(-n)
	}
	const unit = int64(1024)
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := unit, 0
	for q := n / unit; q >= unit && exp < 4; q /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
