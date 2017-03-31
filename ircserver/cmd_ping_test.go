package ircserver

import (
	"testing"

	"github.com/robustirc/robustirc/types"

	"gopkg.in/sorcix/irc.v2"
)

func TestPing(t *testing.T) {
	i, ids := stdIRCServer()

	mustMatchMsg(t,
		i.ProcessMessage(types.RobustId{}, ids["secure"], irc.ParseMessage("PING")),
		":robustirc.net 409 sECuRE :No origin specified")

	mustMatchMsg(t,
		i.ProcessMessage(types.RobustId{}, ids["secure"], irc.ParseMessage("PING foobar")),
		":robustirc.net PONG foobar")
}
