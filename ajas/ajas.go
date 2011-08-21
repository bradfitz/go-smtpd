package main

import (
	"log"
	"os"
	"strings"

	"go-smtpd.googlecode.com/git/smtpd"
)

type env struct {
	*smtpd.BasicEnvelope
}

func (e *env) AddRecipient(rcpt smtpd.MailAddress) os.Error {
	if strings.HasPrefix(rcpt.Email(), "bad@") {
		return os.NewError("we don't send email to bad@")
	}
	return e.BasicEnvelope.AddRecipient(rcpt)
}

func onNewMail(c smtpd.Connection, from smtpd.MailAddress) (smtpd.Envelope, os.Error) {
	log.Printf("ajas: new mail from %q", from)
	return &env{new(smtpd.BasicEnvelope)}, nil
}

func main() {
	s := &smtpd.Server{
		Addr:      ":2500",
		OnNewMail: onNewMail,
	}
	err := s.ListenAndServe()
	if err != nil {
		log.Fatalf("ListenAndServe: %v", err)
	}
}
