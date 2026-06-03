@echo off
:: Build the cloud binary using its package path
go build -ldflags="-s -w -H windowsgui" -o cloud.exe ./cloud

:: Build the cloudgui binary using its package path
go build -ldflags="-s -w -H windowsgui" -o cloudgui.exe ./cloudgui

pause