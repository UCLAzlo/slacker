package slacker

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/shomali11/proper"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

const (
	space               = " "
	dash                = "-"
	star                = "*"
	newLine             = "\n"
	invalidToken        = "invalid token"
	helpCommand         = "help"
	directChannelMarker = "D"
	userMentionFormat   = "<@%s>"
	codeMessageFormat   = "`%s`"
	boldMessageFormat   = "*%s*"
	italicMessageFormat = "_%s_"
	quoteMessageFormat  = ">_*Example:* %s_"
	authorizedUsersOnly = "Authorized users only"
	slackBotUser        = "USLACKBOT"
)

var (
	unAuthorizedError = errors.New("You are not authorized to execute this command")
)

// NewClient creates a new client using the Slack API
func NewClient(botToken, appToken string, options ...ClientOption) (*Slacker, error) {
	defaults := newClientDefaults(options...)

	api := slack.New(
		botToken,
		slack.OptionDebug(defaults.Debug),
		slack.OptionAppLevelToken(appToken),
	)

	info, err := api.AuthTest()
	if err != nil {
		return nil, err
	}

	smc := socketmode.New(
		api,
		socketmode.OptionDebug(defaults.Debug),
	)
	slacker := &Slacker{
		client:                api,
		socketModeClient:      smc,
		commandChannel:        make(chan *CommandEvent, 100),
		unAuthorizedError:     unAuthorizedError,
		botContextConstructor: NewBotContext,
		requestConstructor:    NewRequest,
		responseConstructor:   NewResponse,
		botID:                 info.BotID,
	}
	return slacker, nil
}

// Slacker contains the Slack API, botCommands, and handlers
type Slacker struct {
	client                  *slack.Client
	socketModeClient        *socketmode.Client
	botCommands             []BotCommand
	botLinkShares           []BotLinkShare
	botContextConstructor   func(ctx context.Context, api *slack.Client, client *socketmode.Client, evt *MessageEvent) BotContext
	requestConstructor      func(botCtx BotContext, properties *proper.Properties) Request
	responseConstructor     func(botCtx BotContext) ResponseWriter
	interactiveEventHandler func(botCtx botContext, response ResponseWriter)
	initHandler             func()
	errorHandler            func(err string)
	helpDefinition          *CommandDefinition
	interactionHandler      func(botCtx BotContext, response ResponseWriter, callback_id string, block_id string, action_id string, value string)
	messageHandler          func(botCtx BotContext, response ResponseWriter)
	unAuthorizedError       error
	commandChannel          chan *CommandEvent
	botID                   string
}

// BotCommands returns Bot Commands
func (s *Slacker) BotCommands() []BotCommand {
	return s.botCommands
}

// Client returns the internal slack.Client of Slacker struct
func (s *Slacker) Client() *slack.Client {
	return s.client
}

// SocketMode returns the internal socketmode.Client of Slacker struct
func (s *Slacker) SocketMode() *socketmode.Client {
	return s.socketModeClient
}

// Init handle the event when the bot is first connected
func (s *Slacker) Init(initHandler func()) {
	s.initHandler = initHandler
}

// Err handle when errors are encountered
func (s *Slacker) Err(errorHandler func(err string)) {
	s.errorHandler = errorHandler
}

// CustomRequest creates a new request
func (s *Slacker) CustomRequest(requestConstructor func(botCtx BotContext, properties *proper.Properties) Request) {
	s.requestConstructor = requestConstructor
}

// CustomResponse creates a new response writer
func (s *Slacker) CustomResponse(responseConstructor func(botCtx BotContext) ResponseWriter) {
	s.responseConstructor = responseConstructor
}

// UnAuthorizedError error message
func (s *Slacker) UnAuthorizedError(unAuthorizedError error) {
	s.unAuthorizedError = unAuthorizedError
}

// Help handle the help message, it will use the default if not set
func (s *Slacker) Help(definition *CommandDefinition) {
	s.helpDefinition = definition
}

// Command define a new command and append it to the list of existing commands
func (s *Slacker) Command(usage string, definition *CommandDefinition) {
	s.botCommands = append(s.botCommands, NewBotCommand(usage, definition))
}

// LinkShare define a new link handler and append it to the list of existing link handlers
func (s *Slacker) Link(domain string, definition *LinkShareDefinition) {
	s.botLinkShares = append(s.botLinkShares, NewBotLinkShare(domain, definition))
}

// Interact handles all actions from buttons
func (s *Slacker) Interact(interactionHandler func(botCtx BotContext, response ResponseWriter, callback_id string, block_id string, action_id string, value string)) {
	s.interactionHandler = interactionHandler
}

// Message handle all messages
func (s *Slacker) Message(messageHandler func(botCtx BotContext, response ResponseWriter)) {
	s.messageHandler = messageHandler
}

// CommandEvents returns read only command events channel
func (s *Slacker) CommandEvents() <-chan *CommandEvent {
	return s.commandChannel
}

// Listen receives events from Slack and each is handled as needed
func (s *Slacker) Listen(ctx context.Context) error {
	s.prependHelpHandle()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-s.socketModeClient.Events:
				if !ok {
					return
				}

				switch evt.Type {
				case socketmode.EventTypeConnecting:
					fmt.Println("Connecting to Slack with Socket Mode.")
					if s.initHandler == nil {
						continue
					}
					go s.initHandler()
				case socketmode.EventTypeConnectionError:
					fmt.Println("Connection failed. Retrying later...")
				case socketmode.EventTypeConnected:
					fmt.Println("Connected to Slack with Socket Mode.")

				case socketmode.EventTypeInteractive:

					if s.interactiveEventHandler == nil {
						fmt.Printf("Ignored %+v\n", evt)
						continue
					}
					callback, ok := evt.Data.(slack.InteractionCallback)
					if !ok {
						fmt.Printf("Ignored %+v\n", evt)
						continue
					}
					s.handleInteractionEvent(ctx, &callback)
					s.socketModeClient.Ack(*evt.Request)

				case socketmode.EventTypeSlashCommand:
					ev, ok := evt.Data.(slack.SlashCommand)
					if !ok {
						fmt.Printf("Ignored %+v\n", evt)
						continue
					}
					s.handleCommandEvent(ctx, &ev)
					s.socketModeClient.Ack(*evt.Request)

				case socketmode.EventTypeEventsAPI:
					ev, ok := evt.Data.(slackevents.EventsAPIEvent)
					if !ok {
						fmt.Printf("Ignored %+v\n", evt)
						continue
					}

					switch ev.InnerEvent.Type {
					case slackevents.Message, slackevents.AppMention, slackevents.LinkShared: // message-based events
						go s.handleMessageEvent(ctx, ev.InnerEvent.Data)
					default:
						fmt.Printf("unsupported inner event: %+v\n", ev.InnerEvent.Type)
					}

					s.socketModeClient.Ack(*evt.Request)

				default:
					s.socketModeClient.Debugf("unsupported Events API event received")
				}
			}
		}
	}()

	// blocking call that handles listening for events and placing them in the
	// Events channel as well as handling outgoing events.
	return s.socketModeClient.Run()
}

// GetUserInfo retrieve complete user information
func (s *Slacker) GetUserInfo(user string) (*slack.User, error) {
	return s.client.GetUserInfo(user)
}

func (s *Slacker) defaultHelp(botCtx BotContext, request Request, response ResponseWriter) {
	authorizedCommandAvailable := false
	helpMessage := empty
	for _, command := range s.botCommands {
		tokens := command.Tokenize()
		for _, token := range tokens {
			if token.IsParameter() {
				helpMessage += fmt.Sprintf(codeMessageFormat, token.Word) + space
			} else {
				helpMessage += fmt.Sprintf(boldMessageFormat, token.Word) + space
			}
		}

		if len(command.Definition().Description) > 0 {
			helpMessage += dash + space + fmt.Sprintf(italicMessageFormat, command.Definition().Description)
		}

		if command.Definition().AuthorizationFunc != nil {
			authorizedCommandAvailable = true
			helpMessage += space + fmt.Sprintf(codeMessageFormat, star)
		}

		helpMessage += newLine

		if len(command.Definition().Example) > 0 {
			helpMessage += fmt.Sprintf(quoteMessageFormat, command.Definition().Example) + newLine
		}
	}

	if authorizedCommandAvailable {
		helpMessage += fmt.Sprintf(codeMessageFormat, star+space+authorizedUsersOnly) + newLine
	}
	response.Reply(helpMessage)
}

func (s *Slacker) prependHelpHandle() {
	if s.helpDefinition == nil {
		s.helpDefinition = &CommandDefinition{}
	}

	if s.helpDefinition.Handler == nil {
		s.helpDefinition.Handler = s.defaultHelp
	}

	if len(s.helpDefinition.Description) == 0 {
		s.helpDefinition.Description = helpCommand
	}

	s.botCommands = append([]BotCommand{NewBotCommand(helpCommand, s.helpDefinition)}, s.botCommands...)
}

func (s *Slacker) handleInteractionEvent(ctx context.Context, callback *slack.InteractionCallback) {
	me := &MessageEvent{
		Channel: callback.Channel.ID,
		User:    callback.User.ID,
		Text:    "",
		Data:    callback,
		Type:    string(callback.Type),
	}
	botCtx := s.botContextConstructor(ctx, s.client, s.socketModeClient, me)
	response := s.responseConstructor(botCtx)
	action := callback.ActionCallback.BlockActions[0]

	s.interactionHandler(botCtx, response, callback.CallbackID, action.BlockID, action.ActionID, action.Value)
}

func (s *Slacker) handleCommandEvent(ctx context.Context, evt *slack.SlashCommand) {
	ev := &MessageEvent{
		Channel: evt.ChannelID,
		User:    evt.UserID,
		Text:    evt.Text,
		Data:    evt,
		//Type:            slackevents.SlashCommand,
		//Timestamp:       ev.,
		//ThreadTimeStamp: ev.ThreadTimeStamp,
	}

	botCtx := s.botContextConstructor(ctx, s.client, s.socketModeClient, ev) // note: nil message event
	response := s.responseConstructor(botCtx)

	for _, cmd := range s.botCommands {
		parameters, isMatch := cmd.Match(ev.Text)
		if !isMatch {
			continue
		}

		request := s.requestConstructor(botCtx, parameters)
		if cmd.Definition().AuthorizationFunc != nil && !cmd.Definition().AuthorizationFunc(botCtx, request) {
			response.ReportError(s.unAuthorizedError)
			return
		}

		select {
		case s.commandChannel <- NewCommandEvent(cmd.Usage(), parameters, ev):
		default:
			// full channel, dropped event
		}

		cmd.Execute(botCtx, request, response)
		return
	}
}

func (s *Slacker) handleMessageEvent(ctx context.Context, evt interface{}) {
	ev := newMessageEvent(evt)
	if ev == nil {
		// event doesn't appear to be a valid message type
		return
	}

	if ev.BotID == s.botID {
		// ignore messages this bot posted
		return
	}

	botCtx := s.botContextConstructor(ctx, s.client, s.socketModeClient, ev)
	response := s.responseConstructor(botCtx)

	if linkEvt, ok := ev.Data.(*slackevents.LinkSharedEvent); ok {
		for _, link := range s.botLinkShares {
			for _, domain := range linkEvt.Links {
				if link.Domain() == domain.Domain {
					if value, err := url.Parse(domain.URL); err != nil {
						// bad URL
					} else {
						link.Execute(botCtx, value, response)
					}
				}
			}
		}
	}

	if s.messageHandler != nil {
		s.messageHandler(botCtx, response)
	}
}

func newMessageEvent(evt interface{}) *MessageEvent {
	var me *MessageEvent

	switch ev := evt.(type) {
	case *slackevents.MessageEvent:
		me = &MessageEvent{
			Channel:         ev.Channel,
			User:            ev.User,
			Text:            ev.Text,
			Data:            evt,
			Type:            ev.Type,
			TimeStamp:       ev.TimeStamp,
			ThreadTimeStamp: ev.ThreadTimeStamp,
			BotID:           ev.BotID,
		}
	case *slackevents.AppMentionEvent:
		me = &MessageEvent{
			Channel:         ev.Channel,
			User:            ev.User,
			Text:            ev.Text,
			Data:            evt,
			Type:            ev.Type,
			TimeStamp:       ev.TimeStamp,
			ThreadTimeStamp: ev.ThreadTimeStamp,
			BotID:           ev.BotID,
		}
	case *slackevents.LinkSharedEvent:
		me = &MessageEvent{
			Channel:         ev.Channel,
			User:            ev.User,
			Data:            evt,
			Type:            ev.Type,
			TimeStamp:       string(ev.MessageTimeStamp),
			ThreadTimeStamp: ev.ThreadTimeStamp,
		}
	}

	// Filter out other bots. At the very least this is needed for MessageEvent
	// to prevent the bot from self-triggering and causing loops. However better
	// logic should be in place to prevent repeated self-triggering / bot-storms
	// if we want to enable this later.
	//if me.IsBot() {
	//	return nil
	//}

	return me
}

func actionToMessageEvent(callback *slack.InteractionCallback) *MessageEvent {
	me := &MessageEvent{
		Channel: callback.Channel.ID,
		User:    callback.User.ID,
		Text:    "",
		Data:    callback,
		Type:    string(callback.Type),
	}
	return me
}
