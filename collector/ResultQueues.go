package collector

import "github.com/spitefulgrog/nagflux/data"

type ResultQueues map[data.Target]chan Printable
