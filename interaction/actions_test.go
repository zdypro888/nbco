package interaction

import "testing"

func TestNormalizeActions(t *testing.T) {
	actions := Normalize([]Action{
		{Kind: ActionOpenWebApp, Label: "  周报  ", URL: "https://nbco.example/report"},
		{Kind: ActionOpenWebApp, Label: "重复", URL: "https://nbco.example/report"},
		{Kind: ActionOpenURL, URL: "http://insecure.example"},
		{Kind: "execute", Label: "无效", URL: "https://nbco.example/run"},
	}, 4)
	if len(actions) != 1 || actions[0].Label != "周报" || actions[0].Kind != ActionOpenWebApp {
		t.Fatalf("normalized actions = %+v", actions)
	}
}
