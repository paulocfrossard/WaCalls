package main

// canSeeCall reports whether a stream or request principal identified by
// (role, userID, authDisabled) may see the call record owned by callOwnerKey
// (the principal-scoped bucket on CallRecord, e.g. "user:<userID>" or
// "client:<clientID>").
//
// The rules are closed and exhaustive:
//
//  1. authDisabled (insecure dev mode) → true: there are no principals to
//     separate, so every stream sees every call.
//  2. role == "admin" → true: admins see all calls regardless of owner.
//  3. otherwise → callOwnerKey == "user:"+userID: a non-admin authenticated
//     principal sees only calls whose conflict bucket belongs to its user.
//  4. orphans ("client:<clientID>" buckets) and calls with an empty ownerKey
//     fail rule 3 naturally: they never equal "user:<userID>", so they are
//     admin-only (fail-closed) — no special-casing needed.
func canSeeCall(role, userID string, authDisabled bool, callOwnerKey string) bool {
	if authDisabled {
		return true
	}
	if role == "admin" {
		return true
	}
	return callOwnerKey == "user:"+userID
}
