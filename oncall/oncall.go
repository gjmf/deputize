// deputize - Update an LDAP group with info from the PagerDuty API
// oncall.go: On Call Updater
//
// Copyright 2017 Threat Stack, Inc. All rights reserved.
// Author: Patrick T. Cable II <pat.cable@threatstack.com>

package oncall

import (
  "crypto/tls"
  "crypto/x509"
  "encoding/json"
  "fmt"
  "github.com/nlopes/slack"
  "github.com/PagerDuty/go-pagerduty"
  vault "github.com/hashicorp/vault/api"
  "gopkg.in/ldap.v2"
  "io/ioutil"
  "log"
  "os"
  "reflect"
  "strings"
  "time"
)

// DeputizeConfig is our config struct
type DeputizeConfig struct {
  BaseDN string
  LDAPServer string
  LDAPPort int
  MailAttribute string
  MemberAttribute string
  ModUserDN string
  OnCallGroup string
  OnCallGroupDN string
  OnCallSchedules []string
  RootCAFile string
  SlackChan string
  SlackEnabled bool
  TokenPath string
  VaultSecretPath string
  VaultServer string
}

// UpdateOnCallRotation - read in config and update the on call config.
func UpdateOnCallRotation() error {
  // Configure the things
  var config DeputizeConfig
  var cfile = "config.json"
  jsonConfig, _ := os.Open(cfile)
  decoder := json.NewDecoder(jsonConfig)
  config = DeputizeConfig{}
  err := decoder.Decode(&config)
  if err != nil {
    return fmt.Errorf("Unable to parse config.json: %s", err)
  }

  var currentTime = time.Now()

  // We use vault for storing the LDAP user password, PD token, Slack token
  vaultConfig := vault.DefaultConfig()
  vaultConfig.Address = config.VaultServer
  vaultClient, err := vault.NewClient(vaultConfig)
  if err != nil {
    return fmt.Errorf("Error initializing Vault client: %s\n", err)
  }
  if config.TokenPath == "" {
    if os.Getenv("VAULT_TOKEN") == "" {
      return fmt.Errorf("TokenPath isn't set & no VAULT_TOKEN env present")
    }
  } else {
    vaultToken, err := ioutil.ReadFile(config.TokenPath)
    if err != nil {
      return fmt.Errorf("Unable to read host token from %s", config.TokenPath)
    }
    vaultClient.SetToken(strings.TrimSpace(string(vaultToken)))
  }
  secret, err := vaultClient.Logical().Read("secret/deputize")
  if err != nil {
    return fmt.Errorf("Unable to read secrets from vault: ", config.VaultSecretPath)
  }

  // Begin talking to PagerDuty
  client := pagerduty.NewClient(secret.Data["pdAuthToken"].(string))
  log.Printf("Deputize starting. Oncall groups: %s", strings.Join(config.OnCallSchedules[:],", "))
  var newOnCallEmails []string
  var newOnCallUids []string

  // Cycle through the schedules and once we hit one we care about, get the
  // email address of the person on call for the date period between runtime
  // and runtime+12 hours
  var lsSchedulesOpts pagerduty.ListSchedulesOptions
  if allSchedulesPD, err := client.ListSchedules(lsSchedulesOpts); err != nil {
    return fmt.Errorf("PagerDuty Client says: %s", err)
  } else {
    for _, p := range allSchedulesPD.Schedules {
      if contains(config.OnCallSchedules, p.Name) {
        // We've hit one of the schedules we care about, so let's get the list
        // of on-call users between today and +12 hours.
        var onCallOpts pagerduty.ListOnCallUsersOptions
        onCallOpts.Since = currentTime.Format("2006-01-02T15:04:05Z07:00")
        hours, _ := time.ParseDuration("12h")
        onCallOpts.Until = currentTime.Add(hours).Format("2006-01-02T15:04:05Z07:00")
        log.Printf("Getting oncall for schedule \"%s\" (%s) between %s and %s",
          p.Name, p.APIObject.ID, onCallOpts.Since, onCallOpts.Until)
        if oncall, err := client.ListOnCallUsers(p.APIObject.ID, onCallOpts); err != nil {
            return fmt.Errorf("Unable to ListOnCallUsers: %s", err)
        } else {
          for _, person := range oncall {
            newOnCallEmails = append(newOnCallEmails, person.Email)
          }
        }
      }
    }
  }

  // Now to figure out what LDAP user the email correlates to
  l, err := ldap.Dial("tcp", fmt.Sprintf("%s:%d", config.LDAPServer, config.LDAPPort))
  if err != nil {
    log.Fatal(err)
  }
  defer l.Close()

  // RootCA setup
  tlsConfig := &tls.Config{
      InsecureSkipVerify: false,
      ServerName: config.LDAPServer,
    }

  if config.RootCAFile != "" {
    rootCerts := x509.NewCertPool()
    rootCAFile, err := ioutil.ReadFile(config.RootCAFile)
    if err != nil {
      return fmt.Errorf("Unable to read RootCAFile: %s", err)
    }
    if !rootCerts.AppendCertsFromPEM(rootCAFile) {
      return fmt.Errorf("Unable to append certs")
    }
    tlsConfig.RootCAs = rootCerts
  }

  err = l.StartTLS(tlsConfig)
  if err != nil {
    return fmt.Errorf("Unable to start TLS connection: %s", err)
  }

  // get current members of lg-oncall group (needed for removal later)
  currentOnCall := search(l, config.BaseDN, config.OnCallGroup, []string{config.MemberAttribute})
  currentOnCallUids := currentOnCall.Entries[0].GetAttributeValues(config.MemberAttribute)
  log.Printf("Currently on call (LDAP): %s", strings.Join(currentOnCallUids[:],", "))
  // yeah, we *shouldnt* need to do this, but I want to make sure
  // both slices are sorted the same way so DeepEqual works
  currentOnCallUids = removeDuplicates(currentOnCallUids)

  for _, email := range newOnCallEmails {
    newOnCall := search(l, config.BaseDN, fmt.Sprintf("(%s=%s)", config.MailAttribute, email), []string{"uid"})
    newOnCallUids = append(newOnCallUids, newOnCall.Entries[0].GetAttributeValue("uid"))
  }
  newOnCallUids = removeDuplicates(newOnCallUids)

  log.Printf("New on call (PagerDuty): %s", strings.Join(newOnCallUids[:],", "))

  if reflect.DeepEqual(currentOnCallUids,newOnCallUids) {
    log.Printf("LDAP and PagerDuty match, doing nothing.\n")
  } else {
    log.Printf("Replacing LDAP with PagerDuty information.\n")

    if err := l.Bind(config.ModUserDN, secret.Data["modUserPW"].(string)); err != nil {
      return fmt.Errorf("Unable to bind to LDAP as %s", config.ModUserDN)
    }

    if len(currentOnCallUids) > 0 {
      log.Printf("LDAP: Deleting old UIDs")
      delUsers := ldap.NewModifyRequest(config.OnCallGroupDN)
      delUsers.Delete(config.MemberAttribute, currentOnCallUids)
      if err = l.Modify(delUsers); err != nil {
        return fmt.Errorf("Unable to delete existing users from LDAP")
      }
    }
    log.Printf("LDAP: Adding new UIDs")
    addUsers := ldap.NewModifyRequest(config.OnCallGroupDN)
    addUsers.Add(config.MemberAttribute, newOnCallUids)
    if err = l.Modify(addUsers); err != nil {
      return fmt.Errorf("Unable to add new users to LDAP")
    }

    if config.SlackEnabled == true {
      slackAPI := slack.New(secret.Data["slackAuthToken"].(string))
      slackParams := slack.PostMessageParameters{}
      slackParams.AsUser = true
      slackMsg := fmt.Sprintf("Updated `lg-oncall` on %s: from {%s} to {%s}",
        config.LDAPServer,
        strings.Join(currentOnCallUids[:],", "),
        strings.Join(newOnCallUids[:],", "))
      _,_,err := slackAPI.PostMessage(config.SlackChan, slackMsg, slackParams)
      if err != nil {
        log.Printf("Warning: Got %s back from Slack API\n", err)
      }
    }
  }
  return nil
}