//go:build !race

package memory

func raceDetectorEnabled() bool {
	return false
}
