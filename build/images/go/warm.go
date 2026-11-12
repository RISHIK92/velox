// The packages whose build cache images/go/Dockerfile warms: what the harness (W29) and
// typical solutions import. Anything else still builds, only slower.
package main

import (
	"bufio"
	"container/heap"
	"container/list"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var _ = []any{bufio.NewReader, heap.Init, list.New, json.Marshal, errors.New, fmt.Println, maps.Keys[map[int]int], math.Abs, os.Exit, slices.Sort[[]int], sort.Ints, strconv.Itoa, strings.Split, sync.NewCond, atomic.AddInt64, time.Now}

func main() {}
