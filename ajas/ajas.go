package main

import (
	"errors"
	"log"
	"strings"

	"github.com/kardianos/go-smtpd/smtpd"
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

func onLog(line []byte) error {
	log.Printf("Line: %q", string(line))
	return nil
}

func onNewMail(c smtpd.Connection, from smtpd.MailAddress) (smtpd.Envelope, error) {
	log.Printf("ajas: new mail from %q", from)
	e := new(smtpd.BasicEnvelope)
	e.Log = onLog
	return &env{e}, nil
}

func main() {
	s := &smtpd.Server{
		Addr:      ":2500",
		OnNewMail: onNewMail,
	}
	s.OnProtoError = func(err error) {
		log.Print(err.Error())
	}
	err := s.ListenAndServe()
	if err != nil {
		log.Fatalf("ListenAndServe: %v", err)
	}
}
