package main

// Generated by "go run gentmpl.go status irclog".
// Do not edit manually.

import (
	"html/template"
)

var statusTpl = template.Must(template.New("status").Parse(`<!DOCTYPE html>
<html>
	<head>
		<title>Status of RobustIRC node {{ .Addr }}</title>
		<link rel="stylesheet" href="https://maxcdn.bootstrapcdn.com/bootstrap/3.3.1/css/bootstrap.min.css">
	</head>
	<body>
		<div class="container">
			<div class="page-header">
				<h1>RobustIRC status <small>{{ .Addr }}</small></h1>
			</div>

			<div class="row">
				<div class="col-sm-6">
					<h2>Node status</h2>
					<table class="table">
						<tbody>
							<tr>
								<th>State</th>
								<td>{{ .State }}</td>
							</tr>
							<tr>
								<th>Leader</th>
								<td><a href="https://{{ .Leader }}">{{ .Leader }}</a></td>
							</tr>
							<tr>
								<td class="col-sm-2 field-label"><label>Peers:</label></td>
								<td class="col-sm-10"><ul class="list-unstyled">
								{{ range .Peers }}
									<li><a href="https://{{ . }}">{{ . }}</a></li>
								{{ end }}
								</ul></td>
							</tr>
						</tbody>
					</table>
				</div>

				<div class="col-sm-6">
					<h2>Raft Stats</h2>
					<table class="table table-condensed table-striped">
					{{ range $key, $val := .Stats }}
						<tr>
							<th>{{ $key }}</th>
							<td>{{ $val }}</td>
						</tr>
					{{ end }}
					</table>
				</div>
			</div>

			<div class="row">
				<h2>Active GetMessage requests <span class="badge" style="vertical-align: middle">{{ .GetMessageRequests | len }}</span></h2>
				<table class="table table-striped">
					<thead>
						<tr>
							<th>Session ID</th>
							<th>Nick</th>
							<th>RemoteAddr</th>
							<th>Started</th>
						</tr>
					</thead>
					<tbody>
					{{ range $key, $val := .GetMessageRequests }}
						<tr>
							<td><code>{{ $val.Session.Id | printf "0x%x" }}</code></td>
							<td>{{ $val.Nick }}</td>
							<td>{{ $key }}</td>
							<td>{{ $val.StartedAndRelative }}</td>
						</tr>
					{{ end }}
					</tbody>
				</table>
			</div>

			<div class="row">
				<h2>Active Sessions <span class="badge" style="vertical-align: middle">{{ .Sessions | len }}</span></h2>
				<table class="table table-striped">
					<thead>
						<tr>
							<th></th>
							<th>Session ID</th>
							<th>Last Activity</th>
							<th>Nick</th>
							<th>Channels</th>
						</tr>
					</thead>
					<tbody>
						{{ range .Sessions }}
						<tr>
							<td class="col-sm-1" style="text-align: center"><a href="/irclog?sessionid={{ .Id.Id | printf "0x%x" }}"><span class="glyphicon glyphicon-list"></span></a></td>
							<td class="col-sm-2"><code>{{ .Id.Id | printf "0x%x" }}</code></td>
							<td class="col-sm-2">{{ .LastActivity }}</code></td>
							<td class="col-sm-2">{{ .Nick }}</td>
							<td class="col-sm-7">
							{{ range $key, $val := .Channels }}
							{{ $key }},
							{{ end }}
							</td>
						</tr>
						{{ end }}
					</tbody>
				</table>
			</div>

			<div class="row">
			    <a name="irclog"></a>
				<h2>IRC Log Entries (index={{ .First }} to index={{ .Last}})</h2>
				<a href="/?offset={{ .PrevOffset }}#irclog">Prev</a>
				<a href="/?offset={{ .NextOffset }}#irclog">Next</a>
				<table class="table table-striped">
					<thead>
						<tr>
							<th>Index</th>
							<th>Term</th>
							<th>Type</th>
							<th>Data</th>
						</tr>
					</thead>
					<tbody>
					{{ range .Entries }}
						<tr>
							<td class="col-sm-1">{{ .Index }}</td>
							<td class="col-sm-1">{{ .Term }}</td>
							<td class="col-sm-1">{{ .Type }}</td>
							<td class="col-sm-8"><code>{{ .Data | printf "%s" }}</code></td>
						</tr>
					{{ end }}
					</tbody>
                </table>
			</div>
		</div>
	</body>
</html>
`))

var irclogTpl = template.Must(template.New("irclog").Parse(`<!DOCTYPE html>
<html>
	<head>
		<title>IRC log</title>
		<link rel="stylesheet" href="https://maxcdn.bootstrapcdn.com/bootstrap/3.3.1/css/bootstrap.min.css">
	</head>
	<body>
		<div class="container-fluid">
			<div class="page-header">
				<h1>IRC log <small>{{ .Session.Id | printf "0x%x" }}</small></h1>
			</div>

			<div class="row">
				<table class="table table-striped table-condensed">
					<thead>
						<tr>
							<th>Message ID</th>
							<th>Time</th>
							<th>Text</th>
						</tr>
					</thead>
					<tbody>
						{{ range .Messages }}
						<tr>
							<td class="col-sm-2"><code>{{ .Id.Id }}.{{ .Id.Reply }}</code></td>
							<td class="col-sm-3">{{ .Timestamp }}</td>
							<td class="col-sm-7">
							{{ if eq .Id.Reply 0 }}
							<span class="glyphicon glyphicon-arrow-right"></span>
							{{ else }}
							<span class="glyphicon glyphicon-arrow-left"></span>
							{{ end }}
							<samp>{{ .PrivacyFilter }}</samp></td>
						</tr>
						{{ end }}
					</tbody>
				</table>
			</div>
		</div>
	</body>
</html>
`))
