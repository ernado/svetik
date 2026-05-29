package db

import (
	"math"
	"testing"

	"github.com/stretchr/testify/suite"

	"github.com/ernado/lilith"
)

type MessageTestSuite struct {
	DBTestSuite
}

func (suite *MessageTestSuite) chat() lilith.Chat {
	ctx := suite.T().Context()

	chat := lilith.Chat{
		ID:   1,
		Info: "test chat",
	}

	suite.Require().NoError(suite.db.UpsertChat(ctx, chat))

	return chat
}

func (suite *MessageTestSuite) TestSaveMessage_Insert() {
	ctx := suite.T().Context()

	chat := suite.chat()
	msg := lilith.Message{
		ChatID:    chat.ID,
		MessageID: 100,
		Text:      "hello",
		IsMyself:  false,
	}

	err := suite.db.SaveMessage(ctx, msg)
	suite.Require().NoError(err)

	got, err := suite.db.GetMessage(ctx, msg.ChatID, msg.MessageID)
	suite.Require().NoError(err)
	suite.Equal(msg, *got)
}

func (suite *MessageTestSuite) TestSaveMessage_DoNothingOnConflict() {
	ctx := suite.T().Context()

	chat := suite.chat()
	msg := lilith.Message{
		ChatID:    chat.ID,
		MessageID: 100,
		Text:      "original text",
		IsMyself:  false,
	}

	err := suite.db.SaveMessage(ctx, msg)
	suite.Require().NoError(err)

	msg.Text = "updated text"

	err = suite.db.SaveMessage(ctx, msg)
	suite.Require().NoError(err)

	got, err := suite.db.GetMessage(ctx, msg.ChatID, msg.MessageID)
	suite.Require().NoError(err)
	suite.Equal("original text", got.Text)
}

func (suite *MessageTestSuite) TestSaveMessage_WithReply() {
	ctx := suite.T().Context()

	chat := suite.chat()
	msg := lilith.Message{
		ChatID:      chat.ID,
		MessageID:   200,
		Text:        "reply message",
		IsMyself:    true,
		ReplyToID:   lilith.T(int64(100)),
		ReplyToText: lilith.T("quoted text"),
	}

	err := suite.db.SaveMessage(ctx, msg)
	suite.Require().NoError(err)

	got, err := suite.db.GetMessage(ctx, msg.ChatID, msg.MessageID)
	suite.Require().NoError(err)
	suite.Equal(msg, *got)
}

func (suite *MessageTestSuite) TestGetLastMessages_Empty() {
	ctx := suite.T().Context()

	chat := suite.chat()

	msgs, err := suite.db.GetLastMessages(ctx, chat.ID, 10, math.MaxInt64)
	suite.Require().NoError(err)
	suite.Empty(msgs)
}

func (suite *MessageTestSuite) TestGetLastMessages_LessThanN() {
	ctx := suite.T().Context()

	chat := suite.chat()

	for i := int64(1); i <= 3; i++ {
		err := suite.db.SaveMessage(ctx, lilith.Message{
			ChatID:    chat.ID,
			MessageID: i,
			Text:      "msg",
		})
		suite.Require().NoError(err)
	}

	msgs, err := suite.db.GetLastMessages(ctx, chat.ID, 10, math.MaxInt64)
	suite.Require().NoError(err)
	suite.Require().Len(msgs, 3)
	suite.Equal(int64(1), msgs[0].MessageID)
	suite.Equal(int64(2), msgs[1].MessageID)
	suite.Equal(int64(3), msgs[2].MessageID)
}

func (suite *MessageTestSuite) TestGetLastMessages_ReturnsLastN() {
	ctx := suite.T().Context()

	chat := suite.chat()

	for i := int64(1); i <= 5; i++ {
		err := suite.db.SaveMessage(ctx, lilith.Message{
			ChatID:    chat.ID,
			MessageID: i,
			Text:      "msg",
		})
		suite.Require().NoError(err)
	}

	msgs, err := suite.db.GetLastMessages(ctx, chat.ID, 3, math.MaxInt64)
	suite.Require().NoError(err)
	suite.Require().Len(msgs, 3)
	suite.Equal(int64(3), msgs[0].MessageID)
	suite.Equal(int64(4), msgs[1].MessageID)
	suite.Equal(int64(5), msgs[2].MessageID)
}

func (suite *MessageTestSuite) TestGetLastMessages_AscendingOrder() {
	ctx := suite.T().Context()

	chat := suite.chat()

	for _, id := range []int64{10, 20, 30} {
		err := suite.db.SaveMessage(ctx, lilith.Message{
			ChatID:    chat.ID,
			MessageID: id,
			Text:      "msg",
		})
		suite.Require().NoError(err)
	}

	msgs, err := suite.db.GetLastMessages(ctx, chat.ID, 3, math.MaxInt64)
	suite.Require().NoError(err)
	suite.Require().Len(msgs, 3)
	suite.Equal(int64(10), msgs[0].MessageID)
	suite.Equal(int64(20), msgs[1].MessageID)
	suite.Equal(int64(30), msgs[2].MessageID)
}

func (suite *MessageTestSuite) TestGetLastMessages_LastMessageIDCutoff() {
	ctx := suite.T().Context()

	chat := suite.chat()

	for i := int64(1); i <= 5; i++ {
		err := suite.db.SaveMessage(ctx, lilith.Message{
			ChatID:    chat.ID,
			MessageID: i,
			Text:      "msg",
		})
		suite.Require().NoError(err)
	}

	msgs, err := suite.db.GetLastMessages(ctx, chat.ID, 10, 3)
	suite.Require().NoError(err)
	suite.Require().Len(msgs, 3)
	suite.Equal(int64(1), msgs[0].MessageID)
	suite.Equal(int64(2), msgs[1].MessageID)
	suite.Equal(int64(3), msgs[2].MessageID)
}

func TestMessageTestSuite(t *testing.T) {
	t.Parallel()

	suite.Run(t, new(MessageTestSuite))
}
