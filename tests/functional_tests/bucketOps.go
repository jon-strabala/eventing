package eventing

import (
	"fmt"

	"github.com/couchbase/gocb"
)

type user struct {
	ID        int      `json:"uid"`
	Email     string   `json:"email"`
	Interests []string `json:"interests"`
}

func pumpBucketOps(count int, loop bool, expiry int, delete bool) {
	cluster, _ := gocb.Connect("couchbase://127.0.0.1:12000")
	cluster.Authenticate(gocb.PasswordAuthenticator{
		Username: rbacuser,
		Password: rbacpass,
	})
	bucket, err := cluster.OpenBucket("default", "")
	if err != nil {
		fmt.Println("Bucket open, err:", err)
		return
	}

	u := user{
		Email:     "kingarthur@couchbase.com",
		Interests: []string{"Holy Grail", "African Swallows"},
	}

retriggerBucketOp:
	for i := 0; i < count; i++ {
		u.ID = i
		bucket.Upsert(fmt.Sprintf("doc_id_%d", i), u, uint32(expiry))
		if loop {
			goto retriggerBucketOp
		}
	}

	if delete {
		for i := 0; i < count; i++ {
			bucket.Remove(fmt.Sprintf("doc_id_%d", i), 0)
		}
	}
}
