package db

import (
	"testing"

	"github.com/stretchr/testify/suite"

	"github.com/ernado/svetik"
)

type ChatTestSuite struct {
	DBTestSuite
}

func (suite *ChatTestSuite) TestUpsertChat_Insert() {
	ctx := suite.T().Context()

	chat := lilith.Chat{
		ID:   1,
		Info: "test chat",
	}

	err := suite.db.UpsertChat(ctx, chat)
	suite.Require().NoError(err)

	got, err := suite.db.GetChat(ctx, chat.ID)
	suite.Require().NoError(err)
	suite.Equal(chat, *got)
}

func (suite *ChatTestSuite) TestUpsertChat_Update() {
	ctx := suite.T().Context()

	chat := lilith.Chat{
		ID:   1,
		Info: "original info",
	}

	err := suite.db.UpsertChat(ctx, chat)
	suite.Require().NoError(err)

	chat.Info = "updated info"

	err = suite.db.UpsertChat(ctx, chat)
	suite.Require().NoError(err)

	got, err := suite.db.GetChat(ctx, chat.ID)
	suite.Require().NoError(err)
	suite.Equal(chat, *got)
}

func TestChatTestSuite(t *testing.T) {
	t.Parallel()

	suite.Run(t, new(ChatTestSuite))
}
