package main

import (
	"log"
	"os"
)

func onNewMail(c Connection, from MailAddress) (Envelope, os.Error) {
	return nil, os.NewError("no")
}

func main() {
	s := &Server{
		Addr:      ":2500",
		OnNewMail: onNewMail,
	}
	err := s.ListenAndServe()
	if err != nil {
		log.Fatalf("ListenAndServe: %v", err)
	}
}
