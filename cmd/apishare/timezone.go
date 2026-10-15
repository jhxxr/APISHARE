package main

import "time"

// China Standard Time is UTC+08:00 year-round. A fixed location keeps the
// application's business dates independent of the host's configured timezone.
var beijingLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

func beijingNow() time.Time {
	return time.Now().In(beijingLocation)
}
