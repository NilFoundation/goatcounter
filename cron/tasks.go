// Copyright © 2019 Martin Tournoij <martin@arp242.net>
// This file is part of GoatCounter and published under the terms of the EUPL
// v1.2, which can be found in the LICENSE file or at http://eupl12.zgo.at

package cron

import (
	"context"
	"fmt"

	"zgo.at/goatcounter"
	"zgo.at/goatcounter/acme"
	"zgo.at/goatcounter/cfg"
	"zgo.at/goatcounter/errors"
	"zgo.at/zdb"
	"zgo.at/zhttp/ctxkey"
	"zgo.at/zlog"
)

func DataRetention(ctx context.Context) error {
	var sites goatcounter.Sites
	err := sites.List(ctx)
	if err != nil {
		return err
	}

	for _, s := range sites {
		if s.Settings.DataRetention <= 0 {
			continue
		}

		err = s.DeleteOlderThan(ctx, s.Settings.DataRetention)
		if err != nil {
			zlog.Module("cron").Field("site", s.ID).Error(err)
		}
	}

	return nil
}

func persistAndStat(ctx context.Context) error {
	l := zlog.Module("cron")

	hits, err := goatcounter.Memstore.Persist(ctx)
	if err != nil {
		return err
	}
	if len(hits) > 0 {
		l = l.Since("memstore")
	}

	grouped := make(map[int64][]goatcounter.Hit)
	for _, h := range hits {
		if h.Bot > 0 {
			continue
		}
		grouped[h.Site] = append(grouped[h.Site], h)
	}
	for siteID, hits := range grouped {
		err := UpdateStats(ctx, siteID, hits)
		if err != nil {
			l.Fields(zlog.F{
				"site":  siteID,
				"paths": hits,
			}).Error(err)
		}
	}

	if len(hits) > 100 {
		l.Since("stats").FieldsSince().Printf("persisted %d hits", len(hits))
	}
	return err
}

func UpdateStats(ctx context.Context, siteID int64, hits []goatcounter.Hit) error {
	var site goatcounter.Site
	err := site.ByID(ctx, siteID)
	if err != nil {
		return err
	}
	ctx = context.WithValue(ctx, ctxkey.Site, &site)

	err = updateHitStats(ctx, hits)
	if err != nil {
		return errors.Wrapf(err, "hit_stat: site %d", siteID)
	}
	err = updateBrowserStats(ctx, hits)
	if err != nil {
		return errors.Wrapf(err, "browser_stat: site %d", siteID)
	}
	err = updateLocationStats(ctx, hits)
	if err != nil {
		return errors.Wrapf(err, "location_stat: site %d", siteID)
	}
	err = updateRefStats(ctx, hits)
	if err != nil {
		return errors.Wrapf(err, "ref_stat: site %d", siteID)
	}
	err = updateSizeStats(ctx, hits)
	if err != nil {
		return errors.Wrapf(err, "size_stat: site %d", siteID)
	}

	if !site.ReceivedData {
		_, err = zdb.MustGet(ctx).ExecContext(ctx,
			`update sites set received_data=1 where id=$1`, siteID)
		if err != nil {
			return errors.Wrapf(err, "update received_data: site %d", siteID)
		}
	}
	return nil
}

func ReindexStats(ctx context.Context, hits []goatcounter.Hit, table string) error {
	grouped := make(map[int64][]goatcounter.Hit)
	for _, h := range hits {
		grouped[h.Site] = append(grouped[h.Site], h)
	}

	for siteID, hits := range grouped {
		var site goatcounter.Site
		err := site.ByID(ctx, siteID)
		if err != nil {
			if zdb.ErrNoRows(err) { // Deleted site.
				continue
			}
			return fmt.Errorf("cron.ReindexStats: %w", err)
		}
		ctx = context.WithValue(ctx, ctxkey.Site, &site)

		switch table {
		case "all":
			err = UpdateStats(ctx, siteID, hits)
		case "hit_stats":
			err = updateHitStats(ctx, hits)
		case "browser_stats":
			err = updateBrowserStats(ctx, hits)
		case "location_stats":
			err = updateLocationStats(ctx, hits)
		case "ref_stats":
			err = updateRefStats(ctx, hits)
		case "size_stats":
			err = updateSizeStats(ctx, hits)
		}
		if err != nil {
			return err
		}
	}

	return nil
}

func renewACME(ctx context.Context) error {
	if !acme.Enabled() {
		return nil
	}

	// Don't do this on shutdown as the HTTP server won't be available.
	if stopped.Value() == 1 {
		return nil
	}

	var sites goatcounter.Sites
	err := sites.ListCnames(ctx)
	if err != nil {
		return err
	}

	for _, s := range sites {
		wg.Add(1)
		go func(d string) {
			defer wg.Done()
			err := acme.Make(d)
			if err != nil {
				zlog.Module("cron-acme").Error(err)
			}
		}(*s.Cname)
	}

	return nil
}

func vacuumDeleted(ctx context.Context) error {
	var sites goatcounter.Sites
	err := sites.OldSoftDeleted(ctx)
	if err != nil {
		return err
	}

	for _, s := range sites {
		zlog.Module("vacuum").Printf("vacuum site %s/%d", s.Code, s.ID)

		err := zdb.TX(ctx, func(ctx context.Context, db zdb.DB) error {
			for _, t := range []string{"usage", "browser_stats", "hit_stats", "hits", "location_stats", "ref_stats", "size_stats", "users"} {
				_, err := db.ExecContext(ctx, fmt.Sprintf(`delete from %s where site=%d`, t, s.ID))
				if err != nil {
					return fmt.Errorf("%s: %w", t, err)
				}
			}
			_, err := db.ExecContext(ctx, `delete from sites where id=$1`, s.ID)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func clearSessions(ctx context.Context) error {
	var query string
	if cfg.PgSQL {
		query = `update sessions set hash=null where last_seen > now() + interval '1 hour'`
	} else {
		query = `update sessions set hash=null where last_seen > datetime(datetime(), '+1 hours')`
	}
	_, err := zdb.MustGet(ctx).ExecContext(ctx, query)
	return err
}