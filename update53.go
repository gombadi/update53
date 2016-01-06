package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/route53"
)

type futureAns struct {
	e error  // any error encountered while producing the data
	s string // data to be returned
}

type update53 struct {
	previousip string // previous ip so we don't update every timer tick
	forceip    string // use this ip for the update
	getip      string // get the public ip from this url
	hostname   string // update this hostname
	hostedzone string // use this hosted zone id if known. saves a call to AWS
	daemon     bool   // run as a daeon & update every 5 minutes
	verbose    bool   // print status info
	debug      bool   // print extra debug info
}

// main is the application start point
func main() {

	u53 := &update53{}

	// Flags are set during testing but env used during lambda runs
	flag.StringVar(&u53.forceip, "forceip", "", "Use this ip instead of real ip")
	flag.StringVar(&u53.getip, "getip", "", "get the public ip from this url")
	flag.StringVar(&u53.hostname, "hostname", "", "Hostname to update")
	flag.StringVar(&u53.hostedzone, "hostedzone", "", "HostedZone ID if known")
	flag.BoolVar(&u53.daemon, "daemon", false, "run as a daemon & check every 5 minutes")
	flag.BoolVar(&u53.verbose, "verbose", false, "display status info")
	flag.BoolVar(&u53.debug, "debug", false, "produce extra output")
	flag.Parse()

	if u53.debug == true {
		u53.verbose = true
	}

	if u53.forceip != "" && u53.getip != "" {
		log.Printf("error - can not supply both -forceip and -getip\n")
		os.Exit(1)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	// create time tick channel
	tickChan := time.NewTicker(time.Minute * 5).C
	errChan := make(chan error)

	// do initial update which will cause a result on the errChan
	go u53.updatehostname(errChan)

	dowhile := true
	for dowhile == true {
		select {
		case <-tickChan:
			// send off i am alive message to AWS
			if u53.debug {
				log.Printf("debug - tick Chan triggered\n")
			}
			go u53.updatehostname(errChan)
		case err := <-errChan:
			if err != nil {
				log.Printf("warning - error updating route53: %v\n", err)
			}
		case s := <-sig:
			// done channel closed so exit the select and shutdown the seeder
			if u53.verbose {
				log.Println("\nShutting down on signal:", s)
			}
			dowhile = false
		}
		if u53.daemon == false {
			dowhile = false
		}
	}
	if u53.verbose {
		log.Println("Program Exiting")
	}
}

// updateHostname runs in a goroutine and will update route53 with a new ip if it has changed
// any errors will be returned in the errChan
func (u53 *update53) updatehostname(errChan chan error) {

	if u53.hostname == "" {
		errChan <- errors.New("invalid hostname supplied")
		return
	}

	if !strings.HasSuffix(u53.hostname, ".") {
		u53.hostname = u53.hostname + "."
	}

	if u53.forceip != "" {
		if x := net.ParseIP(u53.forceip); x == nil {
			errChan <- fmt.Errorf("invalid force ip supplied: %s", u53.forceip)
			return
		}
	}

	sess := session.New()
	r53svc := route53.New(sess)

	// fill the channels with future answers to our questions
	ipChan := GetPublicIP(u53.forceip, u53.getip)

	// block until the answers become available
	publicIP := <-ipChan

	if publicIP.e != nil {
		errChan <- publicIP.e
		return
	}

	if u53.previousip == publicIP.ip {
		// ip has not changed so no need to do anything else
		if u53.verbose {
			log.Printf("info - public ip has not changed so not updating\n")
		}
		return
	}

	// if we need to update route53 then we need the hosted zone id
	hzIDChan := getHostedZoneID(r53svc, u53.hostname, u53.hostedzone)
	hzID := <-hzIDChan

	if hzID.e != nil {
		errChan <- hzID.e
		return
	}

	params := &route53.ChangeResourceRecordSetsInput{
		ChangeBatch: &route53.ChangeBatch{
			Changes: []*route53.Change{
				{
					Action: aws.String("UPSERT"),
					ResourceRecordSet: &route53.ResourceRecordSet{
						Name: aws.String(u53.hostname), // host we want to update
						Type: aws.String("A"),
						ResourceRecords: []*route53.ResourceRecord{
							{ // set the new ip address
								Value: aws.String(publicIP.ip),
							},
						},
						TTL: aws.Int64(300),
					},
				},
			},
			Comment: aws.String("Update53"),
		},
		HostedZoneId: aws.String(hzID.s),
	}

	// update the dns records in route53
	_, err := r53svc.ChangeResourceRecordSets(params)

	errChan <- err // which may be nil

	// save a copy so we do not update every timer tick
	if u53.verbose {
		log.Printf("info - updated ip cache. new: %s old: %s\n", publicIP.ip, u53.previousip)
	}
	u53.previousip = publicIP.ip
}

// getHostedZoneId gets the data from AWS so route53 can be updated
func getHostedZoneID(r53svc *route53.Route53, hostname, hz string) chan *futureAns {

	c := make(chan *futureAns)

	go func() {
		defer close(c)
		// if we are supplied with a hosted zone id then just return that in the channel
		if hz != "" {
			c <- &futureAns{s: hz, e: nil}
			return
		}

		// get a list of all hosted zones and get the hostedzoneid
		resp, err := r53svc.ListHostedZones(&route53.ListHostedZonesInput{})
		if err != nil {
			c <- &futureAns{s: "", e: err}
			return
		}

		var hzid *string

		// search the hosted zones till we find the zone for the hostname
		// FIXME - max items = 100 so need to handle big lists
		for _, hz := range resp.HostedZones {

			if strings.HasSuffix(hostname, *hz.Name) {
				// we found the hosted zone so use the id
				hzid = hz.Id
				break
			}
		}

		if hzid == nil {
			c <- &futureAns{s: "", e: errors.New("unable to find hosted domain details")}
		} else {
			c <- &futureAns{s: *hzid, e: nil}
		}
	}()
	return c
}

/*

 */
