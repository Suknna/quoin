package connections

import "os"

func readFileAll(path string) ([]byte, error) {
	return os.ReadFile(path)
}
