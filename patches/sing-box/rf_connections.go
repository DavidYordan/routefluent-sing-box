package box

import (
	"github.com/sagernet/sing-box/common/rfconnection"
	"github.com/sagernet/sing/service"
)

func (s *Box) RFConnectionsSnapshot() []rfconnection.Connection {
	observer := service.FromContext[*rfconnection.Observer](s.ctx)
	if observer == nil {
		return []rfconnection.Connection{}
	}
	return observer.Snapshot()
}
