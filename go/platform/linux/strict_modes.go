//go:build linux

package linux

import (
	"bufio"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode"
)

// privateGroup reads the local account databases without cgo or NSS helpers.
// Do not cache membership: revoking a private group must affect the next check.
func privateGroup(gid uint32) bool {
	passwd, err := os.Open("/etc/passwd")
	if err != nil {
		return false
	}
	defer passwd.Close()
	group, err := os.Open("/etc/group")
	if err != nil {
		return false
	}
	defer group.Close()
	return privateGroupFrom(passwd, group, uint32(os.Geteuid()), gid)
}

func accountID(s string) (uint32, bool) {
	if s == "" || strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 32)
	return uint32(n), err == nil
}

func accountName(s string) bool {
	return s != "" && strings.IndexFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || r == ',' || r == ':'
	}) < 0
}

// privateGroupFrom fails closed on malformed or ambiguous local databases.
// A matching group name never overrides evidence of another group member.
func privateGroupFrom(passwd, group io.Reader, uid, gid uint32) bool {
	username := ""
	users := map[string]bool{}
	uids := map[uint32]bool{}
	primaryUsers := 0
	scanner := bufio.NewScanner(passwd)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) != 7 || !accountName(fields[0]) || users[fields[0]] {
			return false
		}
		userID, validUID := accountID(fields[2])
		primary, validGID := accountID(fields[3])
		if !validUID || !validGID || uids[userID] {
			return false
		}
		users[fields[0]], uids[userID] = true, true
		if primary == gid {
			primaryUsers++
		}
		if userID == uid {
			if primary != gid {
				return false
			}
			username = fields[0]
		}
	}
	if scanner.Err() != nil || username == "" || primaryUsers != 1 {
		return false
	}
	groups := map[string]bool{}
	gids := map[uint32]bool{}
	found := false
	scanner = bufio.NewScanner(group)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) != 4 || !accountName(fields[0]) || groups[fields[0]] {
			return false
		}
		groupID, valid := accountID(fields[2])
		if !valid || gids[groupID] {
			return false
		}
		groups[fields[0]], gids[groupID] = true, true
		members := []string{}
		if fields[3] != "" {
			members = strings.Split(fields[3], ",")
			seen := map[string]bool{}
			for _, member := range members {
				if !accountName(member) || seen[member] {
					return false
				}
				seen[member] = true
			}
		}
		if groupID == gid {
			found = true
			for _, member := range members {
				if member != username {
					return false
				}
			}
		}
	}
	return scanner.Err() == nil && found
}
