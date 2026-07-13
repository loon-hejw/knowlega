package service

import "testing"

func TestChatSessionsPersistInProject(t *testing.T) {
	root := t.TempDir()
	created, err := CreateChatSession(root, "Alpha")
	if err != nil {
		t.Fatal(err)
	}
	read, err := ReadChatSession(root, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.Title != "Alpha" {
		t.Fatalf("title=%q", read.Title)
	}
	sessions, err := ListChatSessions(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != created.ID {
		t.Fatalf("sessions=%+v", sessions)
	}
	if err := DeleteChatSession(root, created.ID); err != nil {
		t.Fatal(err)
	}
	sessions, err = ListChatSessions(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("sessions after delete=%+v", sessions)
	}
}
