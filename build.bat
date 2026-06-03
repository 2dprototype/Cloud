go build -o cloud.exe cloud/main.go
go build -ldflags="-H windowsgui" -o cloudgui.exe cloudgui/main.go
REM pause