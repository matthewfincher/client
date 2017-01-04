// @flow
import React from 'react'
import {Box, Avatar, Text, Usernames, Divider, Icon} from '../../../common-adapters'
import {globalStyles, globalMargins, globalColors} from '../../../styles'

import type {Props} from '.'

const Participants = (props: Props) => (
  <Box style={{...globalStyles.flexBoxColumn}}>
    {props.participants.filter(username => username !== props.you).map(username => (
      <Box key={username} style={rowStyle} onClick={() => props.onShowProfile(username)}>
        <Box style={{...globalStyles.flexBoxRow, alignItems: 'center', flex: 1, marginRight: globalMargins.tiny}}>
          <Avatar size={32} username={username} />
          <Usernames colorFollowing={true} type='Body' users={[{username}]} containerStyle={{marginLeft: 12}} />
          <Text type='Body' style={{marginLeft: 8, flex: 1, color: globalColors.black_40, textAlign: 'right'}}>{props.metaDataMap.getIn([username, 'fullname'], 'Unknown')}</Text>
        </Box>
        <Divider style={{marginLeft: 44}} />
      </Box>
    ))}
    <Box style={{...rowStyle, ...globalStyles.flexBoxRow, alignItems: 'center'}} onClick={() => props.onAddParticipant()}>
      <Icon type='icon-user-add-32' style={{marginRight: 12}} />
      <Text type='BodyPrimaryLink' onClick={() => {}}>Add another participant</Text>
    </Box>
  </Box>
)

const rowStyle = {
  ...globalStyles.flexBoxColumn,
  ...globalStyles.clickable,
  height: globalMargins.large,
  paddingLeft: 20,
  paddingRight: 17,
}

export default Participants
