// Copyright 2014 AdRoll, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package server

import (
	"encoding/base64"
	"time"

	"github.com/AdRoll/hologram/log"
	"github.com/nmcclain/ldap"
	"github.com/peterbourgon/g2s"
	"golang.org/x/crypto/ssh"
)

/*
User represents information about a user stored in the cache.
*/
type User struct {
	Username    string
	SSHKeys     []ssh.PublicKey
	ARNs        []string
	DefaultRole string
}

/*
UserCache implementers provide information about registered users.
*/
type UserCache interface {
	// They also need to implement the SSH key verification interface.
	Authenticator
	Update() error
}

/*
LDAPImplementation implementers provide access to LDAP servers for
operations that Hologram uses.
This interface exists for testing purposes.
*/
type LDAPImplementation interface {
	Search(*ldap.SearchRequest) (*ldap.SearchResult, error)
	Modify(*ldap.ModifyRequest) error
}

/*
ldapUserCache connects to LDAP and pulls user settings from it.
*/
type ldapUserCache struct {
	users             map[string]*User
	groups            map[string][]string
	server            LDAPImplementation
	stats             g2s.Statter
	userAttr          string
	baseDN            string
	enableServerRoles bool
	roleAttribute     string
	defaultRole       string
	defaultRoleAttr   string
}

/*
Update() searches LDAP for the current user set that supports
the necessary properties for Hologram.

TODO: call this at some point during verification failure so that keys that have
been recently added to LDAP work, instead of requiring a server restart.
*/
func (luc *ldapUserCache) Update() error {
	start := time.Now()
	if luc.enableServerRoles {
		groupSearchRequest := ldap.NewSearchRequest(
			luc.baseDN,
			ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
			0, 0, false,
			"(objectClass=groupOfNames)",
			[]string{luc.roleAttribute},
			nil,
		)

		groupSearchResult, err := luc.server.Search(groupSearchRequest)
		if err != nil {
			return err
		}

		for _, entry := range groupSearchResult.Entries {
			dn := entry.DN
			arns := entry.GetAttributeValues(luc.roleAttribute)
			log.Debug("Adding %s to %s", arns, dn)
			luc.groups[dn] = arns
		}
	}

	filter := "(sshPublicKey=*)"
	searchRequest := ldap.NewSearchRequest(
		luc.baseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
		0, 0, false,
		filter, []string{"sshPublicKey", luc.userAttr, "memberOf", luc.defaultRoleAttr},
		nil,
	)

	searchResult, err := luc.server.Search(searchRequest)
	if err != nil {
		return err
	}
	for _, entry := range searchResult.Entries {
		username := entry.GetAttributeValue(luc.userAttr)
		userKeys := []ssh.PublicKey{}
		for _, eachKey := range entry.GetAttributeValues("sshPublicKey") {
			sshKeyBytes, _ := base64.StdEncoding.DecodeString(eachKey)
			userSSHKey, err := ssh.ParsePublicKey(sshKeyBytes)
			if err != nil {
				userSSHKey, _, _, _, err = ssh.ParseAuthorizedKey([]byte(eachKey))
				if err != nil {
					log.Warning("SSH key parsing for user %s failed (key was '%s')! This key will not be added into LDAP.", username, eachKey)
					continue
				}
			}

			userKeys = append(userKeys, userSSHKey)
		}

		userDefaultRole := luc.defaultRole
		arns := []string{}
		if luc.enableServerRoles {
			userDefaultRole = entry.GetAttributeValue(luc.defaultRoleAttr)
			if userDefaultRole == "" {
				userDefaultRole = luc.defaultRole
			}
			for _, groupDN := range entry.GetAttributeValues("memberOf") {
				log.Debug(groupDN)
				arns = append(arns, luc.groups[groupDN]...)
			}
		}

		luc.users[username] = &User{
			SSHKeys:     userKeys,
			Username:    username,
			ARNs:        arns,
			DefaultRole: userDefaultRole,
		}

		log.Debug("Information on %s (re-)generated.", username)
	}

	log.Debug("LDAP information re-cached.")
	luc.stats.Timing(1.0, "ldapCacheUpdate", time.Since(start))
	return nil
}

func (luc *ldapUserCache) Users() map[string]*User {
	return luc.users
}

func (luc *ldapUserCache) _verify(username string, challenge []byte, sshSig *ssh.Signature) (
	*User, error) {
	for _, user := range luc.users {
		for _, key := range user.SSHKeys {
			verifyErr := key.Verify(challenge, sshSig)
			if verifyErr == nil {
				return user, nil
			}
		}
	}

	return nil, nil
}

func (luc *ldapUserCache) Authenticate(username string, challenge []byte, sshSig *ssh.Signature) (
	*User, error) {
	// Loop through all of the keys and attempt verification.
	retUser, _ := luc._verify(username, challenge, sshSig)

	if retUser == nil {
		log.Debug("Could not find %s in the LDAP cache; updating from the server.", username)
		luc.stats.Counter(1.0, "ldapCacheMiss", 1)

		// We should update LDAP cache again to retry keys.
		err := luc.Update()
		if err != nil {
			return nil, err
		}
		return luc._verify(username, challenge, sshSig)
	}
	return retUser, nil
}

/*
	NewLDAPUserCache returns a properly-configured LDAP cache.
*/
func NewLDAPUserCache(server LDAPImplementation, stats g2s.Statter, userAttr string, baseDN string, enableServerRoles bool, roleAttribute string, defaultRole string, defaultRoleAttr string) (*ldapUserCache, error) {
	retCache := &ldapUserCache{
		users:             map[string]*User{},
		groups:            map[string][]string{},
		server:            server,
		stats:             stats,
		userAttr:          userAttr,
		baseDN:            baseDN,
		enableServerRoles: enableServerRoles,
		roleAttribute:     roleAttribute,
		defaultRole:       defaultRole,
		defaultRoleAttr:   defaultRoleAttr,
	}

	updateError := retCache.Update()

	// Start updating the user cache.
	return retCache, updateError
}

type KeysFile interface {
	Search(string) (map[string]interface{}, error)
	Load() error
	Keys() (KeysMap, error)
}

/*
   keysFileUserCache read the file that contains public ssh keys and user info
   .
*/
type keysFileUserCache struct {
	users             map[string]*User
	stats             g2s.Statter
	keysFile          KeysFile
	userAttr          string
	enableServerRoles bool
	roleAttr          string
	defaultRole       string
	defaultRoleAttr   string
}

func (kfuc *keysFileUserCache) Update() error {
	start := time.Now()

	users := map[string]*User{}
	seenRoles := map[[2]string]bool{}

	err := kfuc.keysFile.Load() // Load keys from file
	if err != nil {
		return err
	}
	keys, err := kfuc.keysFile.Keys()

	if err != nil {
		return err
	}

	for key, userData := range keys {
		username := userData[kfuc.userAttr].(string)
		defaultRole, ok := userData[kfuc.defaultRoleAttr].(string)
		if !ok || defaultRole == "" {
			defaultRole = kfuc.defaultRole
		}
		user, found := users[username]
		if !found { // Create a new user in the cache if doesn't exist
			user = &User{
				Username:    username,
				SSHKeys:     []ssh.PublicKey{},
				ARNs:        []string{},
				DefaultRole: defaultRole,
			}
		}

		sshKeyBytes, _ := base64.StdEncoding.DecodeString(key)
		sshKey, err := ssh.ParsePublicKey(sshKeyBytes)
		if err != nil {
			sshKey, _, _, _, err = ssh.ParseAuthorizedKey([]byte(key))
			if err != nil {
				log.Warning("SSH key parsing for user %s failed (key was '%s')! This key will not be added into the keys file.", username, key)
				continue
			}
		}
		user.SSHKeys = append(user.SSHKeys, sshKey)

		if kfuc.enableServerRoles {
			roles := userData[kfuc.roleAttr].([]interface{})
			for _, r := range roles {
				role := r.(string)
				if seenRoles[[2]string{username, role}] {
					continue
				}
				user.ARNs = append(user.ARNs, role)
				seenRoles[[2]string{username, role}] = true
			}
		}

		// Create/Update user
		users[username] = user
	}

	kfuc.users = users

	log.Debug("Keys file information re-cached.")
	kfuc.stats.Timing(1.0, "keysFileCacheUpdate", time.Since(start))

	return nil
}

func (kfuc *keysFileUserCache) Users() map[string]*User {
	return kfuc.users
}

func (kfuc *keysFileUserCache) verify(challenge []byte, sshSig *ssh.Signature) (*User, error) {
	for _, user := range kfuc.users {
		for _, sshKey := range user.SSHKeys {
			if err := sshKey.Verify(challenge, sshSig); err == nil {
				return user, nil
			}
		}
	}
	return nil, nil
}

func (kfuc *keysFileUserCache) Authenticate(username string, challenge []byte, sshSig *ssh.Signature) (*User, error) {
	user, _ := kfuc.verify(challenge, sshSig)

	if user == nil {
		log.Debug("Could not find %s in the keys file cache; updating from the file.", username)
		kfuc.stats.Counter(1.0, "keysFileCacheMiss", 1)

		// We should update keys file cache again to retry keys.
		err := kfuc.Update()
		if err != nil {
			return nil, err
		}
		return kfuc.verify(challenge, sshSig)
	}
	return user, nil
}

func NewKeysFileUserCache(keysFile KeysFile, stats g2s.Statter, enableServerRoles bool, userAttr string, roleAttr string, defaultRole string, defaultRoleAttr string) (*keysFileUserCache, error) {
	kfuc := &keysFileUserCache{
		users:             map[string]*User{},
		stats:             stats,
		keysFile:          keysFile,
		userAttr:          userAttr,
		enableServerRoles: enableServerRoles,
		roleAttr:          roleAttr,
		defaultRole:       defaultRole,
		defaultRoleAttr:   defaultRoleAttr,
	}

	err := kfuc.Update()
	return kfuc, err
}
