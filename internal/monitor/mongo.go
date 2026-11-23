package monitor

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dominickasanga/berth/internal/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// bnum reads a BSON number of any width.
func bnum(v any) float64 {
	switch n := v.(type) {
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case float64:
		return n
	case int:
		return float64(n)
	}
	return 0
}

// bdoc reads an embedded document; the v2 driver returns bson.D for nested documents, bson.M elsewhere.
func bdoc(v any) bson.M {
	switch d := v.(type) {
	case bson.M:
		return d
	case bson.D:
		m := make(bson.M, len(d))
		for _, e := range d {
			m[e.Key] = e.Value
		}
		return m
	}
	return bson.M{}
}

func collectMongo(ctx context.Context, sp Spec, prev Prev, now time.Time) Result {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	u := url.URL{Scheme: "mongodb", Host: net.JoinHostPort(sp.Host, strconv.Itoa(sp.Port)), Path: "/"}
	if sp.Username != "" {
		u.User = url.UserPassword(sp.Username, sp.Password)
	}
	q := url.Values{"directConnection": {"true"}, "appName": {"berth-monitor"}}
	if sp.Username != "" {
		q.Set("authSource", Param(sp.Params, "auth_source", "admin"))
	}
	if strings.EqualFold(Param(sp.Params, "tls", "no"), "yes") {
		q.Set("tls", "true")
	}
	u.RawQuery = q.Encode()
	client, err := mongo.Connect(options.Client().ApplyURI(u.String()).
		SetServerSelectionTimeout(5 * time.Second).SetConnectTimeout(5 * time.Second).SetTimeout(10 * time.Second))
	if err != nil {
		return fail(err)
	}
	defer client.Disconnect(context.Background())

	start := time.Now()
	if err := client.Ping(ctx, nil); err != nil {
		return fail(err)
	}
	r := newResult()
	r.Up = true
	r.Metrics["latency_ms"] = millis(time.Since(start))

	admin := client.Database("admin")
	var ss bson.M
	if err := admin.RunCommand(ctx, bson.D{{Key: "serverStatus", Value: 1}}).Decode(&ss); err != nil {
		r.Info["note"] = "serverStatus is not allowed for this user: only reachability is checked"
		return r
	}
	if v, ok := ss["version"].(string); ok {
		r.Info["version"] = v
	}
	r.Info["uptime"] = humanDuration(bnum(ss["uptime"]))
	if se := bdoc(ss["storageEngine"]); se["name"] != nil {
		r.Info["storage_engine"] = fmt.Sprint(se["name"])
	}
	conns := bdoc(ss["connections"])
	cur, avail := bnum(conns["current"]), bnum(conns["available"])
	r.Metrics["connections_current"] = cur
	if cur+avail > 0 {
		r.Metrics["connections_pct"] = 100 * cur / (cur + avail)
	}
	r.Metrics["mem_resident_bytes"] = bnum(bdoc(ss["mem"])["resident"]) * 1024 * 1024
	ops := 0.0
	for _, v := range bdoc(ss["opcounters"]) {
		ops += bnum(v)
	}
	r.Raw["ops"] = ops
	if v, ok := rate(prev, r.Raw, "ops", now); ok {
		r.Metrics["ops_per_s"] = v
	}

	var ld bson.M
	if err := admin.RunCommand(ctx, bson.D{{Key: "listDatabases", Value: 1}}).Decode(&ld); err == nil {
		dbs := store.Table{Title: "Databases", Columns: []string{"Database", "Size on disk"}}
		if arr, ok := ld["databases"].(bson.A); ok {
			sizes := map[string]float64{}
			var names []string
			for _, d := range arr {
				doc := bdoc(d)
				name := fmt.Sprint(doc["name"])
				sizes[name] = bnum(doc["sizeOnDisk"])
				names = append(names, name)
			}
			for i := 0; i < len(names); i++ { // largest first
				for j := i + 1; j < len(names); j++ {
					if sizes[names[j]] > sizes[names[i]] {
						names[i], names[j] = names[j], names[i]
					}
				}
			}
			for _, n := range names {
				dbs.Rows = append(dbs.Rows, []string{n, humanBytes(sizes[n])})
			}
		}
		r.Metrics["db_size_bytes"] = bnum(ld["totalSize"])
		if len(dbs.Rows) > 0 {
			r.Tables = append(r.Tables, dbs)
		}
	}
	return r
}
