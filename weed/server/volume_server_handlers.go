package weed_server

import (
	"net/http"
	"strings"

	"github.com/chrislusf/seaweedfs/weed/util"

	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/security"
	"github.com/chrislusf/seaweedfs/weed/stats"
)

/*

If volume server is started with a separated public port, the public port will
be more "secure".

Public port currently only supports reads.

Later writes on public port can have one of the 3
security settings:
1. not secured
2. secured by white list
3. secured by JWT(Json Web Token)

*/

func (vs *VolumeServer) privateStoreHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Server", "SeaweedFS Volume "+util.VERSION)
	if r.Header.Get("Origin") != "" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
	}
	switch r.Method {
	case "GET", "HEAD":
		stats.ReadRequest()
		vs.GetOrHeadHandler(w, r)
	case "DELETE":
		stats.DeleteRequest()
		vs.guard.WhiteList(vs.DeleteHandler)(w, r)
	case "PUT", "POST":
		stats.WriteRequest()
		vs.guard.WhiteList(vs.PostHandler)(w, r)
	case "OPTIONS":
		stats.ReadRequest()
		w.Header().Add("Access-Control-Allow-Methods", "PUT, POST, GET, DELETE, OPTIONS")
		w.Header().Add("Access-Control-Allow-Headers", "*")
	}
}

func (vs *VolumeServer) publicReadOnlyHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Server", "SeaweedFS Volume "+util.VERSION)
	if r.Header.Get("Origin") != "" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
	}
	switch r.Method {
	case "GET":
		stats.ReadRequest()
		vs.GetOrHeadHandler(w, r)
	case "HEAD":
		stats.ReadRequest()
		vs.GetOrHeadHandler(w, r)
	case "OPTIONS":
		stats.ReadRequest()
		w.Header().Add("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Add("Access-Control-Allow-Headers", "*")
	}
}

func (vs *VolumeServer) maybeCheckJwtAuthorization(r *http.Request, vid, fid string, isWrite bool) bool {

	var signingKey security.SigningKey

	if isWrite {
		if len(vs.guard.SigningKey) == 0 {
			return true
		} else {
			signingKey = vs.guard.SigningKey
		}
	} else {
		if len(vs.guard.ReadSigningKey) == 0 {
			return true
		} else {
			signingKey = vs.guard.ReadSigningKey
		}
	}

	tokenStr := security.GetJwt(r)
	if tokenStr == "" {
		glog.V(1).Infof("missing jwt from %s", r.RemoteAddr)
		return false
	}

	token, err := security.DecodeJwt(signingKey, tokenStr)
	if err != nil {
		glog.V(1).Infof("jwt verification error from %s: %v", r.RemoteAddr, err)
		return false
	}
	if !token.Valid {
		glog.V(1).Infof("jwt invalid from %s: %v", r.RemoteAddr, tokenStr)
		return false
	}

	if sc, ok := token.Claims.(*security.SeaweedFileIdClaims); ok {
		if sepIndex := strings.LastIndex(fid, "_"); sepIndex > 0 {
			fid = fid[:sepIndex]
		}
		return sc.Fid == vid+","+fid
	}
	glog.V(1).Infof("unexpected jwt from %s: %v", r.RemoteAddr, tokenStr)
	return false
}
