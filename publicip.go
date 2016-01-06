package main

import (
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
)

// PublicIP wraps data on a public ip address
type PublicIP struct {
	ip string
	e  error
}

// getPublicIP reads the instance public ip from metadata or returns the force ip
// is it has been supplied or tries to get the ip from the supplied url.
func GetPublicIP(ip, geturl string) chan *PublicIP {

	c := make(chan *PublicIP)

	go func() {

		defer close(c)

		var err error

		// if we are supplied with a forceip then just return that in the channel
		if ip == "" {
			// get our external ip address so we can add it to the results

			if geturl != "" {
				ip, err = getFromURL(geturl)
			} else {
				ip, err = getFromMetadata()
			}
		}
		if err != nil {
			c <- &PublicIP{ip: "", e: fmt.Errorf("error getting public ip: %v", err)}
			return
		}

		if x := net.ParseIP(ip); x == nil {
			c <- &PublicIP{ip: "", e: fmt.Errorf("unable to parse public ip from: %s", ip)}
		} else {
			c <- &PublicIP{ip: ip, e: nil}
		}
	}()
	return c
}

func getFromMetadata() (string, error) {
	md := ec2metadata.New(session.New())
	return md.GetMetadata("public-ipv4")
}

func getFromURL(url string) (string, error) {

	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("unable to get public ip details from: %s error: %v", url, err)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("unable to read public ip in response: %v", err)
	}
	resp.Body.Close()

	return strings.TrimSpace(string(body)), nil

}

/*

 */
