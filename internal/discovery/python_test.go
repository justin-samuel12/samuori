package discovery

import "testing"

// TestPythonServerStart: the start command is read from the application object an entry module creates, only
// when the project depends on a server that runs it.
func TestPythonServerStart(t *testing.T) {
	main := "from fastapi import FastAPI\n\napp = FastAPI(\n    title=\"Audit\",\n)\n"
	for _, c := range []struct {
		name  string
		files []InputFile
		want  string
	}{
		{"fastapi package", []InputFile{{Path: "backend/requirements.txt", Content: []byte("fastapi==0.115.0\nuvicorn[standard]==0.30.0\n")},
			{Path: "backend/app/main.py", Content: []byte(main)}}, "uvicorn app.main:app --host 0.0.0.0 --port 8000"},
		{"typed object", []InputFile{{Path: "requirements.txt", Content: []byte("fastapi\nuvicorn\n")},
			{Path: "main.py", Content: []byte("api: FastAPI = FastAPI()\n")}}, "uvicorn main:api --host 0.0.0.0 --port 8000"},
		{"flask with gunicorn", []InputFile{{Path: "requirements.txt", Content: []byte("flask\ngunicorn\n")},
			{Path: "app.py", Content: []byte("app = Flask(__name__)\n")}}, "gunicorn --bind 0.0.0.0:8000 app:app"},
		{"no server dependency", []InputFile{{Path: "requirements.txt", Content: []byte("fastapi\n")},
			{Path: "main.py", Content: []byte(main)}}, ""},
		{"no application object", []InputFile{{Path: "requirements.txt", Content: []byte("fastapi\nuvicorn\n")},
			{Path: "main.py", Content: []byte("def create():\n    return FastAPI()\n")}}, ""},
	} {
		r := Analyse(Input{Files: c.files})
		if len(r.Findings) != 1 {
			t.Fatalf("%s: findings = %+v", c.name, r.Findings)
		}
		if got := r.Findings[0].SuggestedCommands["start"]; got != c.want {
			t.Errorf("%s: start = %q; want %q", c.name, got, c.want)
		}
	}
	if MaxContent("backend/app/main.py") <= 0 || MaxContent("backend/app/models.py") != -1 {
		t.Error("only conventional entry modules are read")
	}
}
