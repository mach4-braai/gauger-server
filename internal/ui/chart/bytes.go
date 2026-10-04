package chart

import "fmt"

var byteUnits = []string{"B", "KiB", "MiB", "GiB", "TiB"}

// Bytes formats a byte count in binary units: 512 B, 1.5 KiB, 12 MiB.
func Bytes(v float64) string {
	unit := 0
	for v >= 1024*0.9995 && unit < len(byteUnits)-1 {
		v /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%.0f B", v)
	}
	return short(v) + " " + byteUnits[unit]
}

// BytesPerSecond formats a rate in binary units: 1.5 MiB/s.
func BytesPerSecond(v float64) string { return Bytes(v) + "/s" }
