package ircserver

import "gopkg.in/sorcix/irc.v2"

func init() {
	Commands["OPER"] = &ircCommand{
		Func:      (*IRCServer).cmdOper,
		MinParams: 2,
	}
}

func (i *IRCServer) cmdOper(s *Session, reply *Replyctx, msg *irc.Message) {
	name := msg.Params[0]
	password := msg.Params[1]
	authenticated := false
	for _, op := range i.Config.IRC.Operators {
		if op.Name == name && op.Password == password {
			authenticated = true
			break
		}
	}

	if !authenticated {
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.ERR_PASSWDMISMATCH,
			Params:  []string{s.Nick, "Password incorrect"},
		})
		return
	}

	s.Operator = true
	s.modes['o'] = true

	modestr := "+"
	for mode := 'A'; mode < 'z'; mode++ {
		if s.modes[mode] {
			modestr += string(mode)
		}
	}

	i.sendUser(s, reply, &irc.Message{
		Prefix:  i.ServerPrefix,
		Command: irc.RPL_YOUREOPER,
		Params:  []string{s.Nick, "You are now an IRC operator"},
	})
	i.sendServices(reply,
		i.sendUser(s, reply, &irc.Message{
			Prefix:  i.ServerPrefix,
			Command: irc.MODE,
			Params:  []string{s.Nick, modestr},
		}))
}
