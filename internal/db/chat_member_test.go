package db

import (
	"testing"

	"github.com/stretchr/testify/suite"

	"github.com/ernado/svetik"
)

type ChatMemberTestSuite struct {
	DBTestSuite
}

func (suite *ChatMemberTestSuite) chat() svetik.Chat {
	ctx := suite.T().Context()

	chat := svetik.Chat{
		ID:   1,
		Info: "test chat",
	}

	suite.Require().NoError(suite.db.UpsertChat(ctx, chat))

	return chat
}

func (suite *ChatMemberTestSuite) TestUpsertChatMember_Insert() {
	ctx := suite.T().Context()

	chat := suite.chat()
	member := svetik.ChatMember{
		ChatID:    chat.ID,
		UserID:    42,
		Username:  "johndoe",
		FirstName: "John",
		LastName:  "Doe",
		IsAdmin:   false,
		IsCreator: false,
		Rank:      "",
	}

	err := suite.db.UpsertChatMember(ctx, member)
	suite.Require().NoError(err)

	got, err := suite.db.GetChatMember(ctx, member.ChatID, member.UserID)
	suite.Require().NoError(err)
	suite.Equal(member, *got)
}

func (suite *ChatMemberTestSuite) TestUpsertChatMember_Update() {
	ctx := suite.T().Context()

	chat := suite.chat()
	member := svetik.ChatMember{
		ChatID:    chat.ID,
		UserID:    42,
		Username:  "johndoe",
		FirstName: "John",
		LastName:  "Doe",
		IsAdmin:   false,
		IsCreator: false,
		Rank:      "",
	}

	err := suite.db.UpsertChatMember(ctx, member)
	suite.Require().NoError(err)

	member.Username = "janedoe"
	member.FirstName = "Jane"
	member.IsAdmin = true
	member.Rank = "moderator"

	err = suite.db.UpsertChatMember(ctx, member)
	suite.Require().NoError(err)

	got, err := suite.db.GetChatMember(ctx, member.ChatID, member.UserID)
	suite.Require().NoError(err)
	suite.Equal(member, *got)
}

func TestChatMemberTestSuite(t *testing.T) {
	t.Parallel()

	suite.Run(t, new(ChatMemberTestSuite))
}
