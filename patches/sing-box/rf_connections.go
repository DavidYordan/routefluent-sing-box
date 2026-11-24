package box

import (
	"fmt"
	"github.com/sagernet/sing-box/common/rfconnection"
	"github.com/sagernet/sing-box/common/trafficcontrol"
	"github.com/sagernet/sing/service"
)

func (s *Box) RFConnectionsSnapshot() []rfconnection.Connection {
	observer := service.FromContext[*rfconnection.Observer](s.ctx)
	if observer == nil {
		return []rfconnection.Connection{}
	}
	return observer.Snapshot()
}

func (s *Box) RFActivitySnapshot() (trafficcontrol.RFActivity, error) {
	manager := service.PtrFromContext[trafficcontrol.Manager](s.ctx)
	if manager == nil {
		return trafficcontrol.RFActivity{}, fmt.Errorf("native traffic manager is unavailable")
	}
	return manager.RFActivitySnapshot()
}
