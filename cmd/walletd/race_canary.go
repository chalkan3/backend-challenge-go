package main

import (
	"os"
	"sync"
)

// raceCanary is a deliberate, self-contained data race used to prove that a
// race-instrumented image really is instrumented.
//
// The problem it solves is subtle and worth stating plainly: a race detector
// that reports nothing and a race detector that is not running produce the
// SAME output — silence. So "we ran -race on the replicas and saw no warning"
// is only meaningful if we can first make it WARN. This function does that: it
// races two goroutines on one variable, which the detector is guaranteed to
// catch.
//
// It runs only when WALLET_RACE_CANARY=1, and the binary is only ever built
// with -race by scripts/race-replicas.sh. In a normal build (-race absent) this
// is a few microseconds of pointless work that the canary flag never triggers;
// in production it is dead code behind an env check. It touches no domain, no
// repository, no transport — nothing but a local variable — so it cannot affect
// the service even if someone set the flag by accident.
//
// The intent is inverted on purpose: the caller treats "no race reported" as
// the FAILURE condition for this function. See scripts/race-replicas.sh.
func raceCanary() {
	if os.Getenv("WALLET_RACE_CANARY") != "1" {
		return
	}

	var counter int
	var wg sync.WaitGroup

	// Two goroutines, one variable, no synchronization whatsoever. The race
	// detector must flag this. If it does not, the detector is not active and
	// any subsequent "clean" run is worthless.
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			counter++
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			counter++
		}
	}()
	wg.Wait()

	// Keep the write observable so the compiler cannot elide it.
	_ = counter
}
