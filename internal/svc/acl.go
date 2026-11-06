package svc

// Windows service identity. The service runs as its own virtual account,
// which Windows creates with the service: no password, no profile, and no
// rights beyond what is granted to it here (the config folder) and to every
// local account (reading Program Files, making outbound connections).
const (
	ServiceName    = "meshbridge"
	ServiceAccount = `NT SERVICE\` + ServiceName
)

// Well-known SIDs, so the ACL doesn't depend on the system's language.
const (
	sidSystem             = "*S-1-5-18"
	sidAdministrators     = "*S-1-5-32-544"
	sidUsers              = "*S-1-5-32-545"
	sidAuthenticatedUsers = "*S-1-5-11"
	sidCreatorOwner       = "*S-1-3-0"
	sidEveryone           = "*S-1-1-0"
)

// icaclsArgs locks dir (and everything in it) to SYSTEM and Administrators,
// plus the service account when it exists. The config holds the bridge's
// token, and ProgramData lets every local user read what is created there.
//
// /inheritance:r drops what the folder inherited from ProgramData, the
// grants replace whatever was granted explicitly, and the removals catch
// explicit grants to the broad groups. (OI)(CI) makes new files (state.json,
// the control secret, the log) inherit the same ACL.
func icaclsArgs(dir string, withService bool) []string {
	args := []string{dir, "/inheritance:r",
		"/grant:r", sidSystem + ":(OI)(CI)F", sidAdministrators + ":(OI)(CI)F"}
	if withService {
		args = append(args, ServiceAccount+":(OI)(CI)M")
	}
	return append(args, "/remove:g", sidUsers, sidAuthenticatedUsers, sidCreatorOwner, sidEveryone, "/T", "/C", "/Q")
}
