package main

import (
	"errors"
	"log"
	"strings"

	"github.com/bcmk/go-smtpd/smtpd"
)

type env struct {
	*smtpd.BasicEnvelope
}

func (e *env) AddRecipient(rcpt smtpd.MailAddress) error {
	if strings.HasPrefix(rcpt.Email(), "bad@") {
		return errors.New("we don't send email to bad@")
	}
	return e.BasicEnvelope.AddRecipient(rcpt)
}

func onNewMail(c smtpd.Connection, from smtpd.MailAddress, size *int) (smtpd.Envelope, error) {
	log.Printf("ajas: new mail from %q", from)
	if size != nil {
		log.Printf("ajas: of size %d", *size)
	}
	return &env{new(smtpd.BasicEnvelope)}, nil
}

func main() {
	s := &smtpd.Server{
		Addr:      ":2500",
		OnNewMail: onNewMail,
		MaxSize:   10 * 1024 * 1024,
	}
	err := s.ListenAndServe()
	if err != nil {
		log.Fatalf("ListenAndServe: %v", err)
	}
}
